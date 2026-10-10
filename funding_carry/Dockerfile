# funding-carry runner. Build context: funding_carry/ (Railway Root Directory).
FROM python:3.12-slim
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
WORKDIR /app
COPY requirements.txt .
RUN pip install -r requirements.txt
COPY carry ./carry
COPY tests ./tests
# the image is built only if the tests pass; pytest is removed afterwards
RUN pip install pytest==9.1.1 && python -m pytest -q -p no:cacheprovider tests && \
    pip uninstall -y pytest && rm -rf tests
ENV STATE_FILE=/data/carry_state.json PORT=8080 MODE=paper
EXPOSE 8080
CMD ["python", "-m", "carry", "serve"]
