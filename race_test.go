//go:build race

package websocket

// raceEnabled reports a race-detector build, where sync.Pool drops items at
// random and allocation counts are not meaningful.
const raceEnabled = true
