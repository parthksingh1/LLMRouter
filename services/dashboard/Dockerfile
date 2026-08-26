# syntax=docker/dockerfile:1.7
FROM python:3.11-slim

ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1
WORKDIR /app

COPY services/dashboard/requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt

# The dashboard imports the anomaly detector from benchmarks/common so that the page and the
# committed benchmark result cannot disagree. That shared module is the reason the build context
# reaches outside this directory.
COPY services/dashboard/app.py ./app.py
COPY benchmarks /app/benchmarks
COPY config /app/config

RUN useradd --uid 10001 --no-create-home --shell /usr/sbin/nologin dashboard
USER 10001:10001

EXPOSE 8501
ENV STREAMLIT_SERVER_HEADLESS=true \
    STREAMLIT_BROWSER_GATHER_USAGE_STATS=false \
    PYTHONPATH=/app

HEALTHCHECK --interval=10s --timeout=3s --start-period=15s --retries=5 \
  CMD python -c "import urllib.request;urllib.request.urlopen('http://localhost:8501/_stcore/health')"

CMD ["streamlit", "run", "app.py", "--server.port=8501", "--server.address=0.0.0.0"]
