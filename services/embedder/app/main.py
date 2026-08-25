"""Embedding sidecar.

A deliberately tiny service whose only job is to turn text into vectors for the semantic cache.
It exists as a separate process rather than a Go library for one reason: the good small
embedding models ship as ONNX with a Python tokenizer, and reimplementing that tokenizer in Go
would be a lot of subtle work to save one loopback hop.

Two modes, chosen with EMBEDDER_MODE:

    onnx    all-MiniLM-L6-v2 via fastembed. Real sentence embeddings. The model is baked into
            the container image at build time, so the running service never touches the network.

    hash    a deterministic bag-of-trigrams projection, byte-identical to the Go implementation
            in internal/providers/mock.HashEmbed. It needs no model, no download and no native
            dependencies, which is what lets `make bench` reproduce the cache numbers on a
            machine with nothing but Python installed.

The hash mode is not a toy stand-in that happens to be there: it is the mode the committed
calibration was measured in, precisely because it reproduces anywhere. Switching to onnx
changes the similarity distribution and therefore requires re-running the calibration -- the
service reports its mode on /healthz so a mismatch is visible rather than silent.
"""

from __future__ import annotations

import hashlib
import logging
import math
import os
import time
from contextlib import asynccontextmanager
from typing import Annotated, AsyncIterator, Literal

from fastapi import Depends, FastAPI, HTTPException
from pydantic import BaseModel, Field, field_validator

LOG = logging.getLogger("embedder")

#: all-MiniLM-L6-v2 produces 384-dimensional vectors; the hash mode matches that so the two
#: modes are interchangeable from Qdrant's point of view.
DEFAULT_DIM = 384

#: Cap the batch so one caller cannot pin the process. The gateway embeds a single prompt per
#: request; the batch endpoint exists for the calibration harness.
MAX_BATCH = 256

#: Cap per-text length. Beyond a few thousand characters the embedding stops being informative
#: and the cost is linear, so truncating is better than refusing.
MAX_CHARS = 8000


class Settings:
    """Runtime configuration, read once at start-up."""

    def __init__(self) -> None:
        self.mode: str = os.getenv("EMBEDDER_MODE", "hash").strip().lower()
        self.dim: int = int(os.getenv("EMBEDDING_DIM", str(DEFAULT_DIM)))
        self.model_name: str = os.getenv("EMBEDDER_MODEL", "BAAI/bge-small-en-v1.5")
        if self.mode not in ("onnx", "hash"):
            raise ValueError(f"EMBEDDER_MODE must be onnx or hash, got {self.mode!r}")


class EmbedRequest(BaseModel):
    """POST /embed body."""

    texts: list[str] = Field(..., min_length=1, max_length=MAX_BATCH)

    @field_validator("texts")
    @classmethod
    def _truncate(cls, value: list[str]) -> list[str]:
        return [t[:MAX_CHARS] for t in value]


class EmbedResponse(BaseModel):
    """POST /embed response."""

    vectors: list[list[float]]
    model: str
    dim: int
    mode: Literal["onnx", "hash"]
    latency_ms: float


class HealthResponse(BaseModel):
    """GET /healthz response."""

    status: str
    mode: Literal["onnx", "hash"]
    model: str
    dim: int


class Embedder:
    """Turns text into unit-norm vectors."""

    def __init__(self, settings: Settings) -> None:
        self.settings = settings
        self._model: object | None = None

        if settings.mode == "onnx":
            self._model = self._load_onnx()

    def _load_onnx(self) -> object:
        try:
            from fastembed import TextEmbedding
        except ImportError as exc:  # pragma: no cover - exercised only in a broken image
            raise RuntimeError(
                "EMBEDDER_MODE=onnx requires fastembed. Either install it, or run with "
                "EMBEDDER_MODE=hash, which needs no model and is what the committed cache "
                "calibration was measured with."
            ) from exc

        LOG.info("loading embedding model %s", self.settings.model_name)
        # local_files_only keeps the promise that the running service makes no network calls;
        # the image build is where the model is fetched.
        return TextEmbedding(model_name=self.settings.model_name, local_files_only=True)

    @property
    def model_name(self) -> str:
        return self.settings.model_name if self.settings.mode == "onnx" else "hash-trigram-384"

    def embed(self, texts: list[str]) -> list[list[float]]:
        """Embed a batch of texts."""
        if self.settings.mode == "hash":
            return [hash_embed(t, self.settings.dim) for t in texts]

        assert self._model is not None
        # fastembed yields numpy arrays; convert to plain lists so the response is JSON.
        return [list(map(float, vec)) for vec in self._model.embed(texts)]  # type: ignore[attr-defined]


def hash_embed(text: str, dim: int = DEFAULT_DIM) -> list[float]:
    """Deterministic bag-of-character-trigrams projection, unit-normalised.

    This must stay byte-compatible with HashEmbed in
    services/gateway/internal/providers/mock/provider.go. The two are exercised against each
    other by benchmarks/tests/test_hash_embed_parity.py, because a silent divergence would mean
    the calibrated threshold no longer described the running cache.

    Two paraphrases share most of their trigrams and therefore land close together; two
    questions differing by one word share slightly fewer. That is a weaker signal than a real
    sentence encoder, and the calibration is what turns it into a usable operating point.
    """
    vec = [0.0] * dim
    norm_text = " ".join(text.lower().split())
    if len(norm_text) < 3:
        norm_text = norm_text + "__"

    # Iterate over BYTES, not characters. Go slices strings by byte, so a character-wise loop
    # here would diverge on any non-ASCII input -- silently, and only for some prompts.
    raw = norm_text.encode("utf-8")
    for i in range(len(raw) - 2):
        trigram = raw[i : i + 3]
        h = _fnv1a64(trigram)
        idx = h % dim
        # The top bit selects the sign, which keeps unrelated trigrams from all pushing the
        # same direction and collapsing every vector towards a single point.
        vec[idx] += -1.0 if h & (1 << 63) else 1.0

    magnitude = math.sqrt(sum(x * x for x in vec))
    if magnitude == 0:
        vec[0] = 1.0
        return vec
    return [x / magnitude for x in vec]


def _fnv1a64(data: bytes) -> int:
    """64-bit FNV-1a over raw bytes, matching Go's implementation."""
    h = 0xCBF29CE484222325
    for byte in data:
        h ^= byte
        h = (h * 0x100000001B3) & 0xFFFFFFFFFFFFFFFF
    return h


def cosine(a: list[float], b: list[float]) -> float:
    """Cosine similarity of two vectors. Both are unit-norm here, so this is a dot product."""
    return sum(x * y for x, y in zip(a, b, strict=True))


_state: dict[str, object] = {}


@asynccontextmanager
async def lifespan(_: FastAPI) -> AsyncIterator[None]:
    """Load the model once at start-up rather than on the first request."""
    settings = Settings()
    LOG.info("embedder starting in %s mode", settings.mode)
    _state["settings"] = settings
    _state["embedder"] = Embedder(settings)
    yield
    _state.clear()


def get_embedder() -> Embedder:
    """FastAPI dependency: the process-wide embedder."""
    embedder = _state.get("embedder")
    if embedder is None:  # pragma: no cover - only during start-up races
        raise HTTPException(status_code=503, detail="embedder is not ready")
    assert isinstance(embedder, Embedder)
    return embedder


app = FastAPI(
    title="LLMRouter embedder",
    version="1.0.0",
    lifespan=lifespan,
    docs_url="/docs",
)


@app.get("/healthz", response_model=HealthResponse)
def healthz(embedder: Annotated[Embedder, Depends(get_embedder)]) -> HealthResponse:
    """Liveness and, more usefully, which embedding mode is actually running."""
    return HealthResponse(
        status="ok",
        mode=embedder.settings.mode,  # type: ignore[arg-type]
        model=embedder.model_name,
        dim=embedder.settings.dim,
    )


@app.post("/embed", response_model=EmbedResponse)
def embed(
    body: EmbedRequest,
    embedder: Annotated[Embedder, Depends(get_embedder)],
) -> EmbedResponse:
    """Embed one or more texts."""
    start = time.perf_counter()
    vectors = embedder.embed(body.texts)
    elapsed_ms = (time.perf_counter() - start) * 1000

    return EmbedResponse(
        vectors=vectors,
        model=embedder.model_name,
        dim=embedder.settings.dim,
        mode=embedder.settings.mode,  # type: ignore[arg-type]
        latency_ms=round(elapsed_ms, 3),
    )


@app.post("/similarity")
def similarity(
    body: EmbedRequest,
    embedder: Annotated[Embedder, Depends(get_embedder)],
) -> dict[str, float]:
    """Cosine similarity of exactly two texts.

    A convenience for debugging a suspicious cache hit: paste the two prompts and see the score
    the cache would have compared against the threshold.
    """
    if len(body.texts) != 2:
        raise HTTPException(status_code=400, detail="exactly two texts are required")
    left, right = embedder.embed(body.texts)
    return {"similarity": round(cosine(left, right), 6)}


def _digest(text: str) -> str:
    """sha256 of the normalised text, matching the gateway's prompt fingerprint."""
    return hashlib.sha256(" ".join(text.lower().split()).encode("utf-8")).hexdigest()
