# meat

Abridge a code diff into a **reading diff**.

Humans need to review agent-written code in critical systems.
But models are good now. You don't need to review for style or nil-checks
or imports. You need to review concepts, algorithm choices, architecture.

So meat uses a model to reduce a diff to the important parts.
It shows you the meat.

Install with:

```
go install github.com/simonrw/meat/cmd/meat@latest
```

Run with `meat` to review the latest commit.
It takes git-looking parameters to pick commits to review.

To use an existing ChatGPT-authenticated [Codex
CLI](https://developers.openai.com/codex/cli) session, install Codex, run
`codex login`, and select the provider explicitly.
API-key-authenticated Codex sessions are rejected.

```
meat -model codex main...HEAD
```

The `codex` selector uses meat's built-in default model so cached results remain
stable. Use `codex/<model>` to override it, for example
`meat -model codex/gpt-5.6-sol main...HEAD`. Codex runs ephemerally with a
read-only sandbox; meat retains control of repository reads and its edit-plan
tool loop.

It takes a while to process a commit for reading.
So I suggest you have an agent build `meat` into your devtools so that
it pre-processes it.

Very large diffs are split at file and hunk boundaries and abridged
chunk by chunk (up to a few MB), so one huge commit still produces a
single merged reading diff — it just takes proportionally longer.
