# Build from the extracted contest source archive, not the private project root.
FROM node:24.13.0-bookworm-slim AS frontend
WORKDIR /src/web
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN npm install --global pnpm@11.25.0 && pnpm install --frozen-lockfile
COPY web/contest.html web/vite.contest.config.ts web/tsconfig.json web/tsconfig.contest.json ./
COPY web/src/api.ts web/src/components.tsx web/src/styles.css ./src/
COPY web/src/components/WorkflowPreview.tsx ./src/components/
COPY web/src/contest/main.tsx ./src/contest/
RUN pnpm run build:contest

FROM python:3.12.12-slim-bookworm
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 LANGSMITH_TRACING=false
WORKDIR /app
COPY rag_mvp/requirements-langgraph-lock.txt rag_mvp/
COPY web/contest/requirements.txt web/contest/
RUN pip install --no-cache-dir -r web/contest/requirements.txt && pip check
COPY rag_mvp/__init__.py rag_mvp/retrieval.py rag_mvp/generation.py rag_mvp/workflow.py rag_mvp/
COPY web/contest/app.py web/contest/
COPY --from=frontend /src/web/dist-contest/ web/dist-contest/
USER 10001:10001
EXPOSE 8080
CMD ["sh", "-c", "exec uvicorn web.contest.app:app --host 0.0.0.0 --port \"${PORT:-8080}\" --no-access-log"]
