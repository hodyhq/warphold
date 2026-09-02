// Package health turns report history into a traffic light.
package health

import "time"

const (
	Green   = "green"
	Yellow  = "yellow"
	Red     = "red"
	Unknown = "unknown"
	Revoked = "revoked"

	greenFor  = 26 * time.Hour
	yellowFor = 7 * 24 * time.Hour
)

// Input is what health is computed from.
type Input struct {
	LastOK        *time.Time
	LastRunFailed bool
	Revoked       bool
}

// Status returns green/yellow/red/unknown/revoked.
func Status(in Input, now time.Time) string {
	switch {
	case in.Revoked:
		return Revoked
	case in.LastRunFailed:
		return Red
	case in.LastOK == nil:
		return Unknown
	}
	age := now.Sub(*in.LastOK)
	switch {
	case age < 0:
		// A LastOK in the future means a clock jumped somewhere; reporting
		// green off it would hide an agent that has not run in weeks.
		return Unknown
	case age < greenFor:
		return Green
	case age < yellowFor:
		return Yellow
	default:
		return Red
	}
}
