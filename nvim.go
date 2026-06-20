package main

type BufWritePostData struct {
	Match string `json:"match"`
	Buf   int    `json:"buf"`
	File  string `json:"file"`
	PID   int
}
