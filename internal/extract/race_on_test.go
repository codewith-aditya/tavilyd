//go:build race

package extract

// raceDetector slows the pipeline down by ~10x; timing thresholds are relaxed.
const raceDetector = true
