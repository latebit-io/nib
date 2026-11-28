// Package workspace is the extension that lets an agent work on a
// project's files: a shell, and file tools confined to the project.
package workspace

import (
	"context"
	"fmt"
	"strings"

	"github.com/latebit-io/nib/kit"
	"github.com/latebit-io/nib/kit/extension"
	"github.com/latebit-io/nib/kit/permission"
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
	var shellOptions []bash.Option
	if gate := env.Permission; gate != nil {
		// The shell still refuses risky commands by itself; an approval
		// for one call lifts that for the call, and nothing else does.
		shellOptions = append(shellOptions, bash.ApprovedBy(gate.Approved))
		gate.Register("bash", shellPolicy)
		gate.Register("write_file", pathPolicy)
		gate.Register("edit_file", pathPolicy)
		for _, reader := range []string{"read_file", "glob", "list_dir", "search_project"} {
			gate.Register(reader, permission.Unasked)
		}
	}
	shell := bash.New(env.Dir, shellOptions...)
	tools := append([]kit.Tool{shell}, ft.Tools()...)
	// bash refuses grep and find and names this tool instead.
	tools = append(tools, search.New(env.Dir, ft.Search))
	return extension.Extension{
		Toolset: kit.Toolset{Tools: tools},
		Prompt:  fmt.Sprintf(prompt, env.Root),
		Close:   ft.Close,
	}, nil
}

// shellPolicy shows the command and why the shell would refuse it.
// Allowing it always covers that exact command. Search commands are
// not asked about: the shell redirects them to the search tool itself.
func shellPolicy(args string) permission.Check {
	command := strings.TrimSpace(bash.CommandOf(args))
	if command == "" {
		// Nothing to run: the shell reports the bad arguments.
		return permission.Check{Skip: true}
	}
	class, _ := bash.Classify(command)
	if class == bash.GuardSearch {
		return permission.Check{Skip: true}
	}
	return permission.Check{Detail: command, Reason: class.Reason(), Scope: command}
}

// pathPolicy shows the path of a file change. Allowing it always
// covers the tool, not the one file.
func pathPolicy(args string) permission.Check {
	// Arguments that do not parse are still asked about, without a path.
	return permission.Check{Detail: files.PathOf(args)}
}
