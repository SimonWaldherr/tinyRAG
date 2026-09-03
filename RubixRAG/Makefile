SHELL := /bin/bash

APP_ADDR       ?= :8090
APP_STORAGE    ?= r3-data
# STORAGE_BACKEND selects the vectorStore implementation: "tinysql"
# (default, embedded pure-Go SQL engine) or "sqlite" (modernc.org/sqlite,
# also pure Go / no CGO — see docs/VECTOR_DB.md and vectorstore_sqlite.go).
# Override per-invocation, e.g. `make dev STORAGE_BACKEND=sqlite`.
STORAGE_BACKEND ?= tinysql
# MIGRATE_FROM_BACKEND/MIGRATE_FROM_PATH: source of a one-shot copy into
# STORAGE_BACKEND/APP_STORAGE (see `make migrate`). No default — both must
# be given explicitly since there's no sensible guess for "migrate from".
MIGRATE_FROM_BACKEND ?=
MIGRATE_FROM_PATH    ?=

# BIN is where `build`/`ci` put the compiled binary — a fixed, gitignored
# location instead of go build's default of dropping a multi-MB R3(.exe)
# in the repo root on every invocation.
BIN := bin/r3

# external/* holds personal/unrelated scratch projects (each its own Go
# module, see external/*/go.mod) and is entirely gitignored — never format
# or snapshot it alongside R3's own sources.
GO_FILES := $(shell find . -name '*.go' -not -path './$(APP_STORAGE)/*' -not -path './external/*')

.PHONY: fmt fmt-check vet tidy build release test check ci run dev migrate outdated clean snapshot help menu
.DEFAULT_GOAL := menu

# TARGETS is the single list of argument-less targets — both the
# interactive menu below and the non-tty fallback in `menu` read from it,
# so a new target is added here once instead of drifting between two
# hardcoded lists. `migrate` is deliberately excluded: it requires
# MIGRATE_FROM_BACKEND/MIGRATE_FROM_PATH, which a blind menu selection
# can't supply.
TARGETS := fmt fmt-check vet tidy build release test check ci run dev outdated clean snapshot help

# ── Interaktives Menü ─────────────────────────────────────────────
# PuTTY-Hinweis:
# read -n1 kann Enter als leeren String liefern, weil Newline weiterhin
# als Delimiter behandelt wird. read -N1 liest exakt ein Zeichen.
# Zusätzlich werden "", \r und \n akzeptiert.
define _MENU_SCRIPT
items=($(TARGETS))
descs=("Code formatieren" "Formatierung prüfen (CI, ändert nichts)" "go vet" "go mod tidy" "Projekt bauen ($(BIN))" "Release bauen (kleiner, ohne Debug-Symbole)" "Tests ausführen" "fmt + vet + test" "fmt-check + vet + build + test" "Server starten" "fmt + vet + run verbose" "Abhängigkeiten mit neueren Versionen anzeigen" "Build-Artefakte und Snapshots entfernen" "Snapshot als ZIP sichern" "Hilfe anzeigen")
sel=0
cnt=$${#items[@]}

cleanup() {
  [[ -n "$$old" ]] && stty "$$old" 2>/dev/null || true
  tput cnorm 2>/dev/null || true
}
trap cleanup EXIT INT TERM

draw() {
  tput cup "$$1" 0 2>/dev/null || true

  local i
  for i in "$${!items[@]}"; do
    if (( i == sel )); then
      printf "  \033[1;33m▶  %-12s  \033[0;2m%s\033[0m\n" "$${items[$$i]}" "$${descs[$$i]}"
    else
      printf "     %-12s  \033[2m%s\033[0m\n" "$${items[$$i]}" "$${descs[$$i]}"
    fi
  done
}

old=$$(stty -g 2>/dev/null || true)
stty -icanon -echo min 1 time 0 2>/dev/null || true
tput civis 2>/dev/null || true
clear

printf "\n  \033[1mR3 — Makefile\033[0m\n"
printf "  \033[2m↑/↓ navigieren  ·  Enter ausführen  ·  q abbrechen\033[0m\n\n"
draw 4

while true; do
  k=""
  r=""

  IFS= read -rsN1 k || break

  if [[ "$$k" == $$'\x1b' ]]; then
    IFS= read -rsN2 -t 0.1 r || true
    k="$${k}$${r}"
  fi

  case "$$k" in
    $$'\x1b[A')
      (( sel = (sel - 1 + cnt) % cnt ))
      ;;
    $$'\x1b[B')
      (( sel = (sel + 1) % cnt ))
      ;;
    ""|$$'\r'|$$'\n')
      break
      ;;
    q|Q)
      clear
      exit 0
      ;;
  esac

  draw 4
done

cleanup
trap - EXIT INT TERM
clear

printf "  make %s\n\n" "$${items[$$sel]}"
$(MAKE) "$${items[$$sel]}"
endef
export _MENU_SCRIPT

# ── Snapshot: zip → python3 zipfile → tar.gz ──────────────────────
define _SNAPSHOT_SCRIPT
set -euo pipefail

ts=$$(date +%Y%m%d_%H%M)
excl="$(APP_STORAGE)"
base="r3_$${ts}"

if command -v zip >/dev/null 2>&1; then
  out="$${base}.zip"
  printf "  Erstelle %s …\n" "$$out"

  zip -r "$$out" . \
    -x ".git/*" \
    -x "$${excl}/*" \
    -x "external/*" \
    -x "bin/*" \
    -x "r3_*.zip" \
    -x "r3_*.tar.gz" \
    -q

elif command -v python3 >/dev/null 2>&1; then
  out="$${base}.zip"
  printf "  Erstelle %s mit python3 zipfile …\n" "$$out"

  python3 - "$$out" "$$excl" <<'PY'
import fnmatch
import os
import sys
import zipfile

out, excl = sys.argv[1], sys.argv[2]

with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=6) as zf:
    for root, dirs, files in os.walk("."):
        dirs[:] = [
            d for d in dirs
            if d not in {".git", excl, "external", "bin"}
        ]

        for file in files:
            path = os.path.join(root, file)
            name = path.removeprefix("./")

            if fnmatch.fnmatch(name, "r3_*.zip"):
                continue

            if fnmatch.fnmatch(name, "r3_*.tar.gz"):
                continue

            zf.write(path, name)
PY

else
  out="$${base}.tar.gz"
  printf "  zip/python3 nicht verfügbar — erstelle %s …\n" "$$out"

  tar -czf "$$out" \
    --exclude="./.git" \
    --exclude="./$${excl}" \
    --exclude="./external" \
    --exclude="./bin" \
    --exclude="./r3_*.zip" \
    --exclude="./r3_*.tar.gz" \
    .
fi

printf "  \033[1;32m✓\033[0m  %s\n" "$$out"
endef
export _SNAPSHOT_SCRIPT

# ── Ziele ─────────────────────────────────────────────────────────
menu:
	@if [ -t 0 ]; then \
	  bash -c "$$_MENU_SCRIPT"; \
	else \
	  printf "Verfügbare Ziele: $(TARGETS)\n"; \
	fi

fmt:
	@gofmt -w $(GO_FILES)

# fmt-check verifies formatting without rewriting anything — unlike fmt,
# safe for CI: fails loudly (nonzero exit, offending files listed) instead
# of silently mutating the working tree.
fmt-check:
	@unformatted="$$(gofmt -l $(GO_FILES))"; \
	if [ -n "$$unformatted" ]; then \
	  printf "Nicht formatiert (führe 'make fmt' aus):\n%s\n" "$$unformatted"; \
	  exit 1; \
	fi

vet:
	@go vet ./...

tidy:
	@go mod tidy

# BIN is a fixed, gitignored path (see BIN above) instead of go build's
# default of dropping a multi-MB R3(.exe) in the repo root every time.
build:
	@go build -o $(BIN) .

# release is `build` minus debug info: -ldflags="-s -w" drops the DWARF
# debug tables and symbol table (~35MB -> ~26MB, verified — the single
# biggest lever on binary size short of dropping a dependency), -trimpath
# strips this build machine's absolute file paths from the binary too (a
# minor size/hygiene win, and one less thing leaking local filesystem
# layout into a shipped artifact). Not the default `build` target: losing
# DWARF means a panic stack trace has no file/line info and delve can't
# attach meaningfully, both worth keeping during local dev — use `release`
# only for what actually gets deployed (see build-linux-deploy.bat, which
# uses the same flags for the cross-compiled Linux binary).
release:
	@go build -trimpath -ldflags="-s -w" -o $(BIN) .

test:
	@go test ./...

check: fmt vet test

# ci mirrors check but for automation: verify-only formatting (fmt-check,
# not fmt) plus build, so a pipeline fails on the same things a local
# `make check` would auto-fix instead of silently rewriting files.
ci: fmt-check vet build test

run:
	@go run . -addr "$(APP_ADDR)" -storage-backend "$(STORAGE_BACKEND)" -storage-path "$(APP_STORAGE)"

dev: fmt vet
	@go run . -addr "$(APP_ADDR)" -storage-backend "$(STORAGE_BACKEND)" -storage-path "$(APP_STORAGE)" -verbose

# One-shot copy of every chunk (embeddings included, no re-embedding) from
# MIGRATE_FROM_BACKEND/MIGRATE_FROM_PATH into STORAGE_BACKEND/APP_STORAGE.
# Example: switch from tinySQL to sqlite without losing existing imports:
#   make migrate MIGRATE_FROM_BACKEND=tinysql MIGRATE_FROM_PATH=r3-data \
#                STORAGE_BACKEND=sqlite APP_STORAGE=r3-data.db
migrate:
	@if [ -z "$(MIGRATE_FROM_BACKEND)" ] || [ -z "$(MIGRATE_FROM_PATH)" ]; then \
	  echo "Usage: make migrate MIGRATE_FROM_BACKEND=<tinysql|sqlite> MIGRATE_FROM_PATH=<path> [STORAGE_BACKEND=... APP_STORAGE=...]"; \
	  exit 1; \
	fi
	@go run . -storage-backend "$(STORAGE_BACKEND)" -storage-path "$(APP_STORAGE)" \
	  -migrate-from-backend "$(MIGRATE_FROM_BACKEND)" -migrate-from-path "$(MIGRATE_FROM_PATH)"

# outdated lists direct/indirect dependencies with a newer version
# available (go list marks each such line with "[newer.version]") —
# informational only, doesn't change go.mod/go.sum (see `tidy`/`go get`
# for that).
outdated:
	@upd="$$(go list -u -m all 2>/dev/null | grep '\[')"; \
	if [ -n "$$upd" ]; then echo "$$upd"; else echo "Alle Abhängigkeiten aktuell."; fi

# clean removes build/packaging output: the binary `build`/`ci` produce
# and any snapshot archives — never removes APP_STORAGE (the actual
# knowledge base) or anything git-tracked.
clean:
	@rm -f $(BIN) r3_*.zip r3_*.tar.gz
	@rmdir bin 2>/dev/null || true

snapshot:
	@bash -c "$$_SNAPSHOT_SCRIPT"

# ── Hilfe ─────────────────────────────────────────────────────────
help:
	@printf "\n  \033[1mR3 — Make-Ziele\033[0m\n\n"
	@printf "  \033[1;33m%-12s\033[0m  %s\n" \
	  "fmt"        "gofmt -w auf alle Go-Dateien (außer external/)" \
	  "fmt-check"  "Formatierung prüfen, ohne Dateien zu ändern (CI)" \
	  "vet"        "go vet ./..." \
	  "tidy"       "go mod tidy" \
	  "build"      "go build -o $(BIN) ." \
	  "release"    "go build -trimpath -ldflags=\"-s -w\" -o $(BIN) . (kleiner)" \
	  "test"       "go test ./..." \
	  "check"      "fmt + vet + test" \
	  "ci"         "fmt-check + vet + build + test" \
	  "run"        "Server starten ($(APP_ADDR))" \
	  "dev"        "fmt + vet + run -verbose" \
	  "migrate"    "Chunks samt Embeddings zwischen Backends kopieren" \
	  "outdated"   "Abhängigkeiten mit neueren Versionen anzeigen" \
	  "clean"      "$(BIN) und r3_*.zip/.tar.gz entfernen" \
	  "snapshot"   "Projekt als r3_YYYYMMDD_HHMM.zip sichern" \
	  "help"       "Diese Übersicht"
	@printf "\n  APP_ADDR=%-8s  APP_STORAGE=%-10s  STORAGE_BACKEND=%s\n" "$(APP_ADDR)" "$(APP_STORAGE)" "$(STORAGE_BACKEND)"
	@printf "  STORAGE_BACKEND: tinysql (Standard) oder sqlite (modernc.org/sqlite, kein CGO)\n"
	@printf "  Beispiel: make dev STORAGE_BACKEND=sqlite APP_STORAGE=r3-data.db\n"
	@printf "  Migration: make migrate MIGRATE_FROM_BACKEND=tinysql MIGRATE_FROM_PATH=r3-data STORAGE_BACKEND=sqlite APP_STORAGE=r3-data.db\n\n"