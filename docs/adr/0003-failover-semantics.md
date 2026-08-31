# 3. Mid-stream failover via assistant-prefix continuation

**Status:** Accepted

## Context

A provider can die *after* the client has started reading. The HTTP status is long since 200,
forty tokens are on the wire, and then the upstream connection resets or simply goes quiet.

Ordinary failover does not help. It assumes nothing has been sent yet, so it can retry
transparently. Here the client has already received half an answer, and there are three options:

1. Truncate. The user sees a sentence stop mid-word.
2. Restart. The user sees the answer begin again, differently.
3. Continue. Ask a different model to carry on from what has been sent.

## Decision

Continue where the provider supports it, restart where it does not, and say which happened.

**Detection.** Two failure modes, and the second is the common one:

- *Reset*: the provider sends a chunk carrying an error. Easy.
- *Stall*: the provider sends nothing further. The socket stays open, so a plain read loop waits
  forever. A stall timer resets on every chunk and fires after `stream_stall_timeout_ms`,
  treating silence as death.

**Continuation.** Every delta is accumulated as it is forwarded, so the gateway always knows
exactly what the client has seen. On a break, that text is appended to the request as a trailing
assistant message and sent to the fallback. Both Anthropic's Messages API and OpenAI-compatible
prefill interpret a trailing assistant turn as text to continue.

Capability is declared per provider in `config/providers.yaml`:

| `prefix_continuation` | Providers | Behaviour |
|---|---|---|
| `native` | Anthropic | A trailing assistant message is first-class |
| `prefill` | OpenAI, Mistral, Together | Works, with the caveat below |
| `none` | Google | Cannot continue; restarts |

**The prefill caveat.** With `native`, the model genuinely continues its own turn. With
`prefill`, the model is being told "you said this" about text a *different model* wrote. It
usually continues gracefully; occasionally it restates or subtly changes register, because the
prefix is not in its own voice. This is a real limitation, not a theoretical one, and it is why
the mode is recorded on every failover event rather than smoothed over.

**Restart is announced, not hidden.** When the fallback cannot continue, generation restarts and
the failover event carries `restarted: true`. The client's transcript genuinely contains a
discontinuity; SSE cannot unsend bytes, so the only honest thing is to say so. A restarted
answer is also never written to the cache — storing the second half of a discontinuity would
serve it to the next caller as though it were whole.

**Token accounting is summed across attempts.** The first provider's tokens were really
generated and really cost money. Billing only for the attempt that finished would under-bill;
billing both in full would double-count. The runner sums the usage each attempt reports.

**The client connection is never closed by a provider failure.** Only an exhausted chain or a
client disconnect ends the stream.

## The wire format

Failover metadata rides on a *named* SSE event:

```
event: failover
data: {"attempt":1,"from":"anthropic","to":"openai","model":"gpt-4o",
       "reason":"stream_reset","mode":"continue","tokens_preserved":21,"restarted":false}
```

Every OpenAI SDK reads only unnamed `data:` frames, so a vanilla client sees one uninterrupted
stream and a client that cares can listen. A test walks the wire output and asserts that every
unnamed frame is still a valid `chat.completion.chunk`.

## Consequences

**Measured:** 99.97% completion across 10,000 streams at a 5% mid-stream failure rate and a 1%
stall rate. 488 streams recovered by failing over; 18 had to restart; 3 failed because all three
providers broke on the same request.

The benchmark drives the real runner and the real continuation logic rather than a model of
them. Its first version scaled the stall timeout by the accelerated clock, which made the
timeout 1 ms and reported a 67% completion rate that was entirely Go scheduler jitter being
misread as stalled upstreams. The timeout is now decoupled from the clock and the reason is in
the code, because that mistake is easy to make again.

**Costs.** The gateway buffers the accumulated answer for the life of the stream — bounded by
the response length, but not free at high concurrency. Prefill continuation is best-effort.
Failover multiplies cost: a request that fails over pays for both attempts.

## Alternatives considered

**Truncate and let the client retry.** Simplest, and it pushes the problem to every caller. The
client cannot resume either, so it starts over — the same cost with a worse experience.

**Buffer everything and only send once complete.** Removes the problem by removing streaming.
Time-to-first-token is most of the perceived value.

**Replay the whole conversation to the fallback with no prefix.** That is `restart`, applied
universally. It works, and it wastes the tokens already generated and shows the user a visible
discontinuity every time rather than only where it is unavoidable.

## Revisit when

Providers offer resumable streams with a cursor, which would make this whole mechanism
unnecessary. Or when a cross-provider "continue this text" convention emerges that is better
defined than assistant prefill.
