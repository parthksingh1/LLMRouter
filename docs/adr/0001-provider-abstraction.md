# 1. One `Provider` port for five vendors and a mock

**Status:** Accepted

## Context

The gateway talks to OpenAI, Anthropic, Google, Mistral and Together. Three speak the OpenAI
wire protocol; Anthropic and Google do not. Every one of them can be slow, rate-limit, or die
mid-response, and the demo has to work with none of them reachable.

## Decision

One interface, defined by its consumer in `internal/app`:

```go
type Provider interface {
    Name() string
    Models() []domain.ModelDescriptor
    Chat(ctx, req) (ChatResponse, error)
    ChatStream(ctx, req) (<-chan StreamChunk, error)
    Embed(ctx, req) (EmbedResponse, error)
    HealthCheck(ctx) error
}
```

Three implementations sit behind it: an OpenAI-compatible client shared by three vendors,
bespoke adapters for Anthropic Messages and Gemini `generateContent`, and a mock harness.

Two details carry most of the weight:

**A terminal error arrives as a chunk, not as a closed channel.** `StreamChunk` has an `Err`
field. A provider that dies mid-stream sends a chunk carrying the error and *then* closes; a
provider that finishes normally just closes. Without that distinction the failover machinery
cannot tell "the model finished" from "the pipe broke", and would either fail over on every
successful response or on none of them.

**Cross-provider concerns live in the registry, not the adapters.** Circuit breaking, background
health checks and observed-latency tracking are implemented once. Adapters translate requests and
nothing else, which is why adding a sixth OpenAI-compatible vendor is a YAML entry rather than
code.

## Consequences

The mock is not a test double bolted on afterwards — it satisfies the same interface as the live
adapters, so `make demo` exercises the real router, the real failover state machine and the real
cache. Only the bytes on the wire are simulated.

The cost is that provider-specific features have to be either modelled in the domain or passed
through opaquely. `ChatRequest.Raw` carries the original body so tools, `response_format` and
anything OpenAI ships next reach the vendor untouched — at the price of a request field that is
not type-checked.

`Embed` on the interface is a wart. Anthropic has no embeddings endpoint and returns an error,
which means the interface promises something one implementation cannot do. Splitting `Embedder`
into its own port would be cleaner; it is not worth the churn while exactly one caller uses it.

## Revisit when

A sixth provider needs a genuinely different interaction model — bidirectional streaming, or
server-side conversation state. At that point the single `Provider` port stops describing what
the adapters actually do.
