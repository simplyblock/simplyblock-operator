// Package hostpath carries the pNFS metadata server guest's connections to
// the storage node on its own node through the host (design-pnfs-mds-vm.md
// §8.5).
//
// A storage node's SPDK binds its sockets to its data interface, and a pod's
// connection to its own node's address arrives on the CNI's interface instead,
// so the listener refuses it. A connection the host opens to its own address
// is accepted. The forwarder, in the metadata server pod, takes the guest's
// redirected connection and names its original destination to the proxy,
// which runs on the host's network and opens the connection from there.
package hostpath
