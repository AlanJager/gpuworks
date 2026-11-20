package cluster

// HelperProtocolVersion is the version of the contract between the server and
// the database process it starts: the command-line arguments, the one-line
// announcement, and the output of the offline operations. The database process
// states the version it speaks in its announcement, and the server starts only
// one that speaks this version. A change either side cannot ignore raises it.
const HelperProtocolVersion = 1
