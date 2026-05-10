// Package contracttest provides reusable test fixtures that verify a
// concrete implementation upholds a kit port's documented contract.
//
// Each port has one entry-point function — [Provider], [Store], [Tool],
// [SessionEventSink], [Highlighter] — that takes *testing.T plus a
// constructor for the implementation under test, then runs every
// contract claim from the port's doc comment as a named subtest. The
// pattern mirrors [testing/fstest.TestFS] for [io/fs.FS] and the
// database/sql driver test suite.
//
// A new third-party implementation can opt in with one import and one
// test call:
//
//	func TestMyStore(t *testing.T) {
//	    contracttest.Store(t, func() memory.Store {
//	        return mystore.New(t.TempDir())
//	    })
//	}
//
// Each subtest constructs a fresh implementation via the ctor so
// behaviour cannot bleed across cases. Implementations are expected to
// be in the documented "ready for next call" state when ctor returns —
// the fixtures do not perform setup beyond the ctor.
//
// Discipline: when a port's doc comment grows or changes a contract
// claim, the corresponding subtest here must grow or change with it.
// Documenting an invariant and writing the test that enforces it is
// one atomic unit. CodeRabbit and human review enforce the pairing.
package contracttest
