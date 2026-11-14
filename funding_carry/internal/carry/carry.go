// Package carry holds the runner's identity (carry/__init__.py) and the UTC time formats every module prints.
package carry

import "time"

// Day is the UTC day "2006-01-02" of an epoch-ms timestamp.
func Day(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }

// ISO is the UTC time "2006-01-02T15:04:05Z" of an epoch-ms timestamp.
func ISO(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05Z") }

// NowISO is the current UTC time in the ISO format.
func NowISO() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

// NowMS is the current epoch time in ms.
func NowMS() int64 { return time.Now().UnixMilli() }

const (
	Version      = "0.2.0"
	Runner       = "funding"
	StrategyID   = "funding.carry"
	StrategyName = "Funding carry (spot long + perp short)"
)
