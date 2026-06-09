---
description: Walk through a file, function, or concept carefully — explanation only, no edits.
aliases: [ex]
---
Explain: $ARGUMENTS

Approach this as if onboarding a new engineer who is sharp but unfamiliar with the
codebase. Stay in explanation mode; do not edit files, do not run formatters or
linters, do not propose code changes. If something is genuinely broken, name it and
move on — fixing it is a separate request.

When the target is a file or function:
1. Read it before describing it. Quote the specific lines you reference (file:line).
2. Lead with the *why* — what problem this code exists to solve, what it would
   break if it disappeared. Then describe the *what* and *how*.
3. Identify the non-obvious bits: hidden invariants, concurrency assumptions,
   error-handling shortcuts, surprising allocations, anything that would trip a
   reader who hasn't lived with the code.
4. Map it to the surrounding architecture. Who calls in? Who does it call out to?
   Where does it sit in the layering rules from CLAUDE.md / architecture docs?

When the target is a concept or design choice:
1. State the choice in one sentence.
2. List what was rejected, with one-sentence reasons. If you don't know, say so —
   do not invent rationale.
3. Surface the tradeoffs being lived with today.

Output rules:
- No sycophancy. If a piece of code is confused, say so. If it's elegant, say
  why specifically — "this is good" is not a sentence.
- No filler. Skip restating the request. Skip "great question."
- Use code blocks for quoted code; never paraphrase what a function does when
  you could quote it.
- If the explanation would benefit from a small diagram, draw it in ASCII or
  Mermaid. If it wouldn't, don't.
- Stop when the explanation is complete. Do not append a "let me know if..."
  closer.
