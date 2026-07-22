package nvme

import "encoding/binary"

// NVMe Identify Controller data structure (CNS 01h): a fixed 4096-byte blob
// from the Identify admin command. atlas decodes only MNAN; the notable
// fields and their little-endian byte offsets are tabulated for reference.
// See the NVMe Base Specification, "Identify Controller data structure".
//
//	offset  size  field    meaning
//	------  ----  -------  ------------------------------------------------
//	    76     1  CMIC     multi-path I/O & namespace-sharing capabilities
//	    77     1  MDTS     maximum data transfer size (2^n * min page size)
//	    78     2  CNTLID   controller id
//	    80     4  VER      NVMe version
//	   256     2  OACS     optional admin command support
//	   512     1  SQES     submission queue entry size
//	   513     1  CQES     completion queue entry size
//	   514     2  MAXCMD   maximum outstanding commands
//	   516     4  NN       number of namespaces (largest valid NSID)
//	   520     2  ONCS     optional NVM command support (discard, copy, ...)
//	   525     1  VWC      volatile write cache present
//	   536     4  SGLS     SGL support
//	   540     4  MNAN     maximum number of allowed namespaces  (decoded)
//	   768   256  SUBNQN   NVM subsystem NQN
const (
	// identifyControllerLen is the size of the Identify Controller structure.
	identifyControllerLen = 4096
	// mnanOffset is the byte offset of MNAN (uint32, little-endian).
	mnanOffset = 540
)

// mnanFromIdentify decodes MNAN (Maximum Number of Allowed Namespaces) from a
// raw Identify Controller buffer. SPDK sets MNAN to the subsystem's
// max_namespaces, so a value > 1 marks a multi-namespace subsystem. A buffer
// too short to contain the field yields 0.
func mnanFromIdentify(buf []byte) uint32 {
	if len(buf) < mnanOffset+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(buf[mnanOffset : mnanOffset+4])
}
