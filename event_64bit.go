//go:build arm64 || amd64

package main

type inputEventRaw struct {
	Sec  int64
	Usec int64
	Type uint16
	Code uint16
	Val  int32
}
