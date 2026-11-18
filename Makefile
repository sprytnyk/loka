.PHONY: all run check lint test format

all: check

run:
	uv run python -m loka

check: lint test

lint:
	uv run ruff check .
	uv run ruff format --check .

test:
	uv run pytest

format:
	uv run ruff format .
