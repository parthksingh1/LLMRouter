# syntax=docker/dockerfile:1.7
FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

WORKDIR /app

COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

COPY app ./app

# Bake the ONNX model into the image at BUILD time.
#
# This is what keeps the promise that the running stack makes no network calls: the model is
# fetched once, here, and the container starts with local_files_only. A runtime download would
# also make `make demo` fail on a train.
ARG EMBEDDER_MODEL=BAAI/bge-small-en-v1.5
ENV EMBEDDER_MODEL=${EMBEDDER_MODEL} \
    FASTEMBED_CACHE_PATH=/opt/models
RUN python -c "from fastembed import TextEmbedding; TextEmbedding(model_name='${EMBEDDER_MODEL}')" \
 && chmod -R a+rX /opt/models

RUN useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin embedder
USER 10001:10001

EXPOSE 8000
ENV EMBEDDER_MODE=onnx EMBEDDING_DIM=384

HEALTHCHECK --interval=10s --timeout=5s --start-period=25s --retries=6 \
  CMD python -c "import urllib.request;urllib.request.urlopen('http://localhost:8000/healthz')"

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000", "--no-access-log"]
