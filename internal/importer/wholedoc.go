package importer

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// A file that holds two documents imports as one of them.
//
// Both of this package's decoders read the first value and stop. A JSON decoder leaves the rest of the
// stream unread, and yaml.Unmarshal decodes the first document of a stream and ignores the others. Two
// exports appended together therefore imported in part, with a summary that counted only what it had
// read and so agreed with itself: "total 3, comes across 3, does not come across 0" for a file holding
// fifty-three objects, exit zero, no warning. Appending is how a shop with several projects produces
// one file, so this is the ordinary shape and not an abuse of the format.
//
// It was quietly worse than a partial import. The unread-field guard, which exists to name data this
// importer never looked at, unmarshaled the same bytes and returned nothing when they did not parse
// whole. So the file with the most unseen data in it was the one file the guard said nothing about.
//
// The rule lives here once and every entry point calls it, because it held for five of the six formats
// by accident rather than on purpose: json.Unmarshal refuses a tail, and the formats that were right
// were the ones that happened to still use it. AWX moved to a decoder for number precision and lost
// the refusal as a side effect nobody remarked on, and Rundeck's YAML never had it.

// refuseJSONTail reports an error when data carries anything but whitespace after its first JSON value.
//
// A scalar first value is not a refusal. `puppet node list` prints bare certnames one per line, and
// that file is not JSON at all; its own parser says so better than this can. Only an object or an array
// is a document shape, and only those are held to being the whole file.
func refuseJSONTail(data []byte) error {
	reader := bytes.NewReader(data)
	dec := json.NewDecoder(reader)
	var first json.RawMessage
	if err := dec.Decode(&first); err != nil {
		return nil
	}
	if trimmed := bytes.TrimSpace(first); len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return nil
	}
	rest, err := io.ReadAll(io.MultiReader(dec.Buffered(), reader))
	if err != nil {
		return nil
	}
	tail := bytes.TrimSpace(rest)
	if len(tail) == 0 {
		return nil
	}
	return moreThanOneDocument("with " + oneLine(clipLine(string(tail))))
}

// refuseYAMLTail reports an error when data holds more than one YAML document.
//
// The line is named rather than the content, because a YAML document separator sits on its own line and
// the number is what an editor takes. Two sequences concatenated without a separator are one longer
// sequence, which is legal and complete, so that shape is not a finding.
func refuseYAMLTail(data []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil {
		return nil
	}
	var second yaml.Node
	if err := dec.Decode(&second); err != nil {
		return nil
	}
	return moreThanOneDocument(fmt.Sprintf("at line %d", second.Line))
}

// refuseXMLTail reports an error when data holds a second root element.
//
// XML permits exactly one root, so two config.xml files appended together are not a document at all.
// Go's decoder reads the first element and stops, which made that file import as its first job: two
// jobs in, one out, no warning. A Jenkins bundle is still one root, <jobs> with a <job> per directory,
// so the shape the CLI builds is unaffected.
func refuseXMLTail(data []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	first, err := nextElement(dec)
	if err != nil {
		return nil
	}
	if err := dec.Skip(); err != nil {
		return nil
	}
	end := int(dec.InputOffset())
	if end < 0 || end > len(data) {
		return nil
	}
	rest := bytes.TrimSpace(data[end:])
	if len(rest) == 0 {
		return nil
	}

	// Scanned with a fresh decoder rather than by reading on with the one above. Jenkins writes a
	// declaration at the top of every config.xml, so two of them appended put a declaration in the
	// middle of the file; the original decoder stops on that with a syntax error, and treating an error
	// as "nothing follows" is what let the realistic shape through while the declaration-free one was
	// refused. A fresh decoder starts at the tail, where that declaration is legal, and names it.
	//
	// A comment or whitespace after the root element is legal and complete, so only an element or a
	// declaration is a finding. The tail is trimmed first because an XML declaration is only legal with
	// nothing before it, and the newline the exporter wrote after the root element is something.
	tailDec := xml.NewDecoder(bytes.NewReader(rest))
	for {
		tok, err := tailDec.Token()
		switch {
		case errors.Is(err, io.EOF):
			// Comments and whitespace only. The document ended where it said it did.
			return nil
		case err != nil:
			return moreThanOneDocument(fmt.Sprintf("after the first <%s>, with %s",
				first, oneLine(clipLine(string(rest)))))
		}
		switch t := tok.(type) {
		case xml.StartElement:
			return moreThanOneDocument(fmt.Sprintf("with a second <%s> element after the first <%s>",
				t.Name.Local, first))
		case xml.ProcInst:
			return moreThanOneDocument(fmt.Sprintf("with a second document's <?%s?> declaration after "+
				"the first <%s>", t.Target, first))
		}
	}
}

// nextElement returns the name of the next element a decoder reaches.
func nextElement(dec *xml.Decoder) (string, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		if start, ok := tok.(xml.StartElement); ok {
			return start.Name.Local, nil
		}
	}
}

// moreThanOneDocument is the one refusal both encodings give, so an operator reads the same sentence
// whichever format they are importing and the fix named is the same fix.
func moreThanOneDocument(where string) error {
	return fmt.Errorf("%w: this file holds more than one document, and only the first would be "+
		"imported, so the report would count part of the estate as all of it. The second begins %s. "+
		"Export in one pass rather than appending exports together, then import again",
		ErrNothingRecognized, where)
}
