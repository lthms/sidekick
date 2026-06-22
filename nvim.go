package main

type BufWritePostData struct {
	Buf  int    `json:"buf"`
	File string `json:"file"`
	PID  int
}
