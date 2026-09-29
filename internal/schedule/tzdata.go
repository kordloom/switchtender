package schedule

// The zone database travels with the binary.
//
// A schedule names an IANA zone and this package resolves it with time.LoadLocation, which reads the
// operating system's copy. The published container is Alpine, which ships no copy at all, so every
// zone but UTC was rejected there: "unknown timezone \"America/New_York\", use an IANA name such as
// America/New_York", which is a refusal a reader cannot act on because it names the thing it just
// refused. The timezone field, the daylight saving corrections, and per-zone firing were all
// unusable in the image we publish, on every release that had them.
//
// The tests that cover those corrections call LoadLocation too, and skipped when it failed. A skip
// reports as a pass, so the suite was green precisely in the environment where the feature was
// broken, and the one condition that would have exposed it was the condition it excused itself for.
//
// Importing time/tzdata embeds the database and uses it only when the system has none, so this costs
// about half a megabyte and changes nothing on a host that carries its own. It belongs here rather
// than in main because the product is not the only thing that has to resolve a zone: this package's
// own tests do, the importer does through the schedules it builds, and a fix that lived in main
// would leave both of those reading whatever the machine happened to have.
import _ "time/tzdata"
