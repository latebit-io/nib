module github.com/latebit-io/nib/kit

go 1.26

require (
	github.com/latebit-io/nib/agent v0.0.0
	github.com/latebit-io/nib/ai v0.0.0
)

replace (
	github.com/latebit-io/nib/agent => ../agent
	github.com/latebit-io/nib/ai => ../ai
)
