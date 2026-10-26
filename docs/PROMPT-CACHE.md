# Lay out prompts so the cache is reused

Every prefix cache — the one on the card, a host-memory tier, a cache shared between workers — matches a new
request against what it already holds **from the first token**, and **stops at the first token that differs**.
Everything after that point is computed again, at full prefill cost, even if the rest of the prompt is identical.

```
cached:   [system prompt ........][tools ......][document ..........][question A]
new:      [system prompt ........][tools ......][document ..........][question B]
           └────────────── reused ─────────────────────────────────┘ └ computed ┘

new:      [2026-10-23 09:14][system prompt ........][tools ......][document ....][question B]
           └ differs ─────┘ └──────────── all of it computed again ─────────────────────────┘
```

So the order of a prompt's parts decides how much of it is free. This holds on every engine opod serves
(vLLM, SGLang, llama.cpp, TensorRT-LLM) and for every cache tier.

## The rule: stable first, changing last

Put the parts that are the same for many requests at the start, and the parts that change at the end:

1. The system prompt.
2. Tool definitions, **always in the same order**.
3. Shared reference material: a policy, a manual, a retrieved document that many requests use.
4. The conversation so far — append new turns; do not rewrite earlier ones.
5. Last: what changes per request — the user's name, the date, a request id, the question.

## What breaks reuse without anyone noticing

- **A timestamp, request id or user name at the top** of the system prompt. Move it to the end of the last message.
- **Tools in a different order** from one request to the next (a map iterated in random order, a list built from a
  set). Sort them by name once.
- **JSON whose key order changes.** Serialise with a fixed key order (sorted keys) wherever a structure is part of
  the prompt.
- **Whitespace or formatting that varies** — trailing spaces, different line endings, a template that sometimes adds a
  blank line. The cache compares tokens, not meaning.
- **The chat template itself.** Some templates insert today's date, or a random id, into the system header. Then
  reuse breaks once a day (or every request). Read the model's chat template for anything that is not your text.
- **Retrieved documents in a different order.** If several documents are retrieved, order them by a stable key, and
  keep the most shared ones first.

## Who shares a cache

An endpoint separates its callers' caches by team unless its policy says otherwise (`routing.cacheIsolation`:
`team`, `key` or `none`). Two keys of one team reuse each other's prefixes; two teams never do, even with the same
system prompt, so the first request of each team computes it once. On llama.cpp, which has no salt, every caller
shares whatever the setting.

## How to see whether it works

The engines count prefix-cache hits and queries; opod's leader reports the hit rate on `/loadz` as
`prefix_hit_pct`, and the control plane shows it on the endpoint page (*Load*). A long average prompt with a low hit
rate means the prompts share little prefix: check the order of their parts first. A hit rate that drops at the same
time every day usually means a date in the chat template.

opod never rewrites or reads your prompts to judge them: the advice comes from the engines' own counters.
