package common

import "time"

// APISeedStatus expose seed informations
type APISeedStatus struct {
	Name       string
	File       string
	Ready      bool
	URL        string
	Size       uint64
	StatusTime time.Time
	Status     string
}
