// Package nfsclient reads what the kernel's NFS client says about its own
// mounts, from /proc/self/mountstats.
//
// It is the only place a pNFS client reports how it moved data. A mount that
// got layouts and one that sends every byte through the metadata server look
// the same from outside: the mount works and the data lands either way. The
// per-operation counters tell them apart, the layout types say whether the
// mount can use layouts at all, and the transport's connect count says when the
// client reconnected to its server, which is how a server restart shows on the
// client. The CSI node and the test suites both need those readings, which is
// why they are parsed here once.
package nfsclient
