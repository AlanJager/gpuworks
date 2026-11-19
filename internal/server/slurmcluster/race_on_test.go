//go:build race

package slurmcluster

// raceDetector: the race detector slows the code it checks several times
// over, so a CPU budget does not apply.
const raceDetector = true
