package main

import (
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// Errors heroProblem reports, one per way the home page hero image can regress.
var (
	errHeroMissing = errors.New("hero image not found on the home page")
	errHeroSize    = errors.New("hero image width and height attributes are not half its pixel size")
	errHeroAspect  = errors.New("hero image aspect is outside 1.1 to 1.6")
	errHeroLoading = errors.New("hero image is not loaded eagerly at high priority")
)

// heroImg matches the home page hero's img element.
var heroImg = regexp.MustCompile(`<img\s[^>]*src="/assets/screenshot-fleet\.png[^"]*"[^>]*>`)

// heroAttr matches one name="value" attribute inside an element.
var heroAttr = regexp.MustCompile(`\s([a-z]+)="([^"]*)"`)

// heroProblem checks the home page's hero img against the decoded size of the image it shows. The
// image is captured at device scale 2, so its attributes are its pixel size halved; an aspect
// outside 1.1 to 1.6 is the wide strip that rendered table text at about 4 px, or a stretched frame.
// The hero is the page's largest paint, so it must not wait on lazy loading.
func heroProblem(page []byte, img image.Config) error {
	tag := heroImg.Find(page)
	if tag == nil {
		return errHeroMissing
	}
	attrs := map[string]string{}
	for _, m := range heroAttr.FindAllSubmatch(tag, -1) {
		attrs[string(m[1])] = string(m[2])
	}
	if attrs["loading"] != "eager" || attrs["fetchpriority"] != "high" {
		return fmt.Errorf("%w: loading=%q fetchpriority=%q", errHeroLoading, attrs["loading"], attrs["fetchpriority"])
	}
	w, werr := strconv.Atoi(attrs["width"])
	h, herr := strconv.Atoi(attrs["height"])
	if werr != nil || herr != nil || w*2 != img.Width || h*2 != img.Height {
		return fmt.Errorf("%w: attributes %q x %q, image %d x %d", errHeroSize, attrs["width"], attrs["height"],
			img.Width, img.Height)
	}
	if aspect := float64(img.Width) / float64(img.Height); aspect < 1.1 || aspect > 1.6 {
		return fmt.Errorf("%w: %.2f", errHeroAspect, aspect)
	}
	return nil
}

// TestHomeHeroImageFitsItsSlot fails when the published hero image and its markup disagree, or when
// the image is a shape the hero column cannot show readably. The rows after the first are the
// regressions it guards against, the first of them the strip the home page shipped before.
func TestHomeHeroImageFitsItsSlot(t *testing.T) {
	t.Parallel()
	site := filepath.Join("..", "..", "site")
	page, err := os.ReadFile(filepath.Join(site, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(site, "assets", "screenshot-fleet.png"))
	if err != nil {
		t.Fatal(err)
	}
	published, err := png.DecodeConfig(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	tag := func(w, h int, loading string) []byte {
		return fmt.Appendf(nil, `<img src="/assets/screenshot-fleet.png?v=1" width="%d" height="%d" loading="%s" `+
			`fetchpriority="high">`, w, h, loading)
	}
	tests := []struct {
		Page []byte
		Img  image.Config
		Want error
	}{{ // Test 0: The published page and asset agree and fit the slot.
		Page: page, Img: published, Want: nil,
	}, { // Test 1: The full-app strip, 1444 x 560, which rendered at 0.32 scale.
		Page: tag(722, 280, "eager"), Img: image.Config{Width: 1444, Height: 560}, Want: errHeroAspect,
	}, { // Test 2: Attributes that do not match the image reserve the wrong box.
		Page: tag(692, 500, "eager"), Img: image.Config{Width: 1384, Height: 1034}, Want: errHeroSize,
	}, { // Test 3: Lazy loading delays the page's largest paint.
		Page: tag(692, 517, "lazy"), Img: image.Config{Width: 1384, Height: 1034}, Want: errHeroLoading,
	}, { // Test 4: A page without the hero image.
		Page: []byte(`<img src="/assets/other.png">`), Img: published, Want: errHeroMissing,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := heroProblem(test.Page, test.Img)
			if !errors.Is(got, test.Want) || (test.Want == nil && got != nil) {
				t.Errorf("heroProblem = %v, want %v", got, test.Want)
			}
		})
	}
}
