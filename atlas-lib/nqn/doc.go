// Package nqn builds and parses NVMe Qualified Names so the operator and CSI
// driver derive them the same way: subsystem NQNs from a volume identity, and
// host NQNs from a node identity.
//
// The host side matters for the same reason as the subsystem side, only more
// sharply: a host NQN is an authorization subject. The operator registers one
// in an access-controlled pool's allowed_hosts and the CSI driver presents one
// on connect, and a volume mounts only if the two agree character for
// character — so both derive it from Host rather than each spelling out the
// format.
package nqn
