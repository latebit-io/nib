// Package contracttest provides reusable test fixtures that verify a
// concrete implementation upholds an engine port's documented contract.
//
// Mirrors [github.com/latebit-io/nib/kit/contracttest] for the
// engine-side ports ([SessionEventSink], [Highlighter]). The kit
// module does not import engine (engine sits to the side of the
// foundation→kit→coding→tui spine, consumed by coding and tui), so
// fixtures for engine ports live here rather than under kit.
//
// Each fixture takes *testing.T plus a constructor for the
// implementation under test, then runs every contract claim from the
// port's doc comment as a named subtest. The pattern mirrors
// [testing/fstest.TestFS] for [io/fs.FS].
package contracttest
