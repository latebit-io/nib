// Package workspace is the extension that lets an agent work on a
// project's files: a shell, and file tools confined to the project.
package workspace

import (
	"context"
	"fmt"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/tools/bash"
	"github.com/latebit-io/nib/kit/tools/files"
	"github.com/latebit-io/nib/kit/tools/search"
)

// Name is what configuration and flags call the extension.
const Name = "workspace"

// prompt is the extension's part of the system prompt. %s is the project root.
const prompt = `## Workspace

You work in the project at %s. The file tools (read_file, write_file, edit_file, glob, list_dir, search_project) cannot reach outside it. Find text with search_project and files with glob. Read a file before you edit it, and prefer edit_file over rewriting a whole file. Use bash to build, test and inspect.`

// Spec returns the extension, on by default: nearly every session
// reads or changes files.
func Spec() extension.Spec {
	return extension.Spec{
		Name:        Name,
		Description: "a shell, and file and search tools confined to the project",
		Default:     true,
		New:         build,
	}
}

func build(_ context.Context, env extension.Env) (extension.Extension, error) {
	ft, err := files.New(env.Root, env.Dir)
	if err != nil {
		return extension.Extension{}, err
	}
	tools := append([]kit.Tool{bash.New(env.Dir)}, ft.Tools()...)
	// bash refuses grep and find and names this tool instead.
	tools = append(tools, search.New(env.Dir, ft.Search))
	return extension.Extension{
		Toolset: kit.Toolset{Tools: tools},
		Prompt:  fmt.Sprintf(prompt, env.Root),
		Close:   ft.Close,
	}, nil
}
