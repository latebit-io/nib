module github.com/latebit-io/nib/cmd/nibster

go 1.26

require (
	github.com/latebit-io/nib/ai v0.0.0
	github.com/latebit-io/nib/kit v0.0.0
)

require github.com/latebit-io/nib/agent v0.0.0 // indirect

replace (
	github.com/latebit-io/nib/agent => ../../agent
	github.com/latebit-io/nib/ai => ../../ai
	github.com/latebit-io/nib/kit => ../../kit
)
