//go:build arm || 386

package main

type inputEventRaw struct {
	Sec  int32
	Usec int32
	Type uint16
	Code uint16
	Val  int32
}
