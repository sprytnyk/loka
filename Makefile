.PHONY: all run check lint test format build release

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

build:
	uv run --locked --group build pyinstaller loka.spec --clean --noconfirm

release: build
	uv run --no-sync python scripts/package_release.py
