import os

from fastapi import FastAPI

app = FastAPI(title="EdgeProxy FastAPI demo")
INSTANCE = os.getenv("INSTANCE", "api-unknown")


@app.get("/health")
def health():
    return {"status": "ok", "instance": INSTANCE}


@app.get("/api/items")
def items():
    return {"instance": INSTANCE, "items": ["edge", "proxy", "fastapi"]}


@app.get("/")
def root():
    return {"instance": INSTANCE}
