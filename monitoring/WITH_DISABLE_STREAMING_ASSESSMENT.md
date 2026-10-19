# Setting `WithDisableStreaming` on `chatBlocking` — impact assessment

**Status: assessment only. No code changed.**

Today the server never sets `smolllm.WithDisableStreaming`. A client's
`stream:false` is served by *streaming upstream and buffering*: `Ask` opens a
streaming request, `consumeStream` drains it, and the handler renders one
`chat.completion` object. The `streaming` metric label is therefore always
`"true"` on this host.

Setting the flag would make `stream:false` a genuinely blocking call. Four
surfaces change. Two are clear wins, one is a wash, one is a real regression
that needs a decision.

## 1. First-output guard — the one real regression

On the streaming path the guard bounds *time to first byte*: the timer is
armed when the request is built and disarmed at the first delta
(`stream_api.go:166`). A provider that accepts the connection and then says
nothing is cut off after `first_output_timeout`.

On the blocking path the guard is disarmed **the moment headers arrive**
(`ask.go:106`), before a single body byte is read:

```go
if opts.DisableStreaming {
    exec.disarmFirstOutput()
    cr, err = consumeBlocking(resp.Body, exec.start)
}
```

So a blocking leg has no first-output protection at all. If a provider returns
`200` and then stalls mid-body, the request hangs until `request_timeout`
(300s) — not until `first_output_timeout` (10s). For a proxy whose whole point
is fast failover, this converts a 10-second failover into a 5-minute one.

*This is not a code defect to patch on the way past.* The asymmetry is
inherent to the transport: a blocking response has no first output to observe
separately from its last. Bounding it would mean a stall-detector on body
reads, which is a different mechanism from the guard.

**Conclusion: adopting the flag on `chatBlocking` trades away first-output
failover for the requests that take that path.**

## 2. Retry — a wash, with one small gain

`withRetry` retries only `EmptyStreamError` and retryable HTTP statuses
(`retry.go:34-48`). `FirstOutputTimeoutError` is deliberately not retryable.

- **Empty responses:** the check that produces `EmptyStreamError` lives at
  `ask.go:134`, *after* the transport branch, so it fires on both paths
  unchanged. `consumeBlocking` also returns `no choices` on a 200 with an
  empty choices array. No gain, no loss.
- **Small gain:** `stream_options` only exists on streaming requests
  (`transport.go:76` sets `IncludeStreamUsage: !opts.DisableStreaming`). A
  blocking request cannot be rejected by a provider for not supporting it, so
  the `retryWithoutStreamUsage` special case (`transport.go:347`) becomes dead
  code for this path. Today that retry *costs a full extra round trip and bills
  a duplicate leg* when a provider dislikes `stream_options`.
- **Small loss:** the same reasoning means a blocking request can no longer
  recover from that rejection, because it never provokes it. Net: one fewer way
  to fail, no new one.

## 3. Latency — worse for real users

Two separate effects, both negative:

- **No time-to-first-byte delivery.** Today a `stream:false` client gets the
  whole answer at once either way, so this does not change the client's
  experience — but the *proxy* stops being able to forward partial progress,
  and the caller waits for the full generation with no bytes on the wire. Any
  intermediary or client with its own idle timeout sees a silent connection.
- **TTFT becomes meaningless.** `ask.go:180` pins `cr.ttft = total` for
  blocking calls, by design. The histogram would then record *call duration*
  under the name `attempt_first_output_seconds`. That is exactly the mixing
  the `streaming` label was introduced to prevent — the label would work, but
  the population it separates would have no latency signal left in it.

## 4. Credit accounting — unchanged, and this is the reassuring part

Credit is derived from `RequestEvent.RawUsage` (`internal/server/credit.go:28`),
which both transports populate from the provider's `usage`. `consumeBlocking`
reuses `usageChunk`, so the three prompt-cache dialects normalise identically
(`blocking.go:11-13`). Measured against the live `agent` alias:

| Mode | credit | complete | prompt_tokens | cache_read |
|---|---|---|---|---|
| `stream:true` | 0 | true | 5 | 0 |
| `stream:false` | 0 | true | 5 | 0 |

`attempts` and `known` counts also carry over unchanged, so
`credit_complete` keeps its meaning. One nuance: the `retryWithoutStreamUsage`
path emits a failure hook for the rejected request, which counts as an attempt
— but that path cannot trigger without `stream_options`, so blocking requests
would report *fewer* attempts, i.e. a cleaner `credit_attempts`.

## Recommendation

**Do not set the flag.** The gains are cosmetic (a label value that appears in
no user-visible behaviour, plus dead-code elimination on a retry path) and the
cost is real: 10s first-output failover becomes a 300s hang, and the latency
histogram loses its only meaningful reading for that population.

If the underlying goal is *"the `streaming` label should have two values"*,
that goal is not worth this trade. The label is already correct and already
doing its job — it is honest that this host has one transport mode. The useful
follow-up, if any, is to document that `stream:false` still streams upstream,
so nobody reads `streaming="true"` as "the client asked for streaming".

If a genuine blocking transport is wanted later, it should come with a
body-stall detector to replace the guard, as its own change.
