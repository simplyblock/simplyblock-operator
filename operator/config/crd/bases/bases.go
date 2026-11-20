// The CRDs controller-gen writes into this directory, embedded for the Go code
// that reads them.
//
// An embed directive cannot reach outside the directory of the file that
// declares it, so the declaration sits next to the generated files instead of
// in the package that uses them. That keeps a single committed copy of every CRD in the
// operator module: the upgrade tool imports this package rather than carrying a
// copy of its own.

package bases

import "embed"

// FS holds every generated CRD. The pattern is deliberately narrow: a stray
// file in the directory would otherwise be embedded and then fail to parse as
// a CRD at run time, which is a failure the build should have had.
//
//go:embed *.yaml
var FS embed.FS
