#!/usr/bin/env bash
# Producer for eod2 (GPL-3.0). Keeps a separate checkout, runs its daily sync,
# and leaves CSVs for `verdict ingest eod2 --dir "$EOD2_DIR/src/eod2_data"`.
# Nothing from eod2 is imported into this repository.
set -euo pipefail

EOD2_DIR="${EOD2_DIR:-$HOME/.local/share/eod2}"
# Apple's system Python, deliberately. Homebrew's python3.13 and python3.14 on this
# machine cannot create a virtualenv at all: their pyexpat.so wants the symbol
# _XML_SetAllocTrackerActivationThreshold, which /usr/lib/libexpat.1.dylib does not
# export, so ensurepip dies and the venv comes out without pip. /usr/bin/python3
# (3.9.6) has a working pyexpat and installs every eod2 requirement cleanly.
EOD2_PYTHON="${EOD2_PYTHON:-/usr/bin/python3}"

if [ ! -d "$EOD2_DIR/.git" ]; then
  echo "cloning eod2 with its ~1.6 GB data submodule into $EOD2_DIR (one-time, several minutes)"
  git clone --recurse-submodules https://github.com/BennyThadikaran/eod2.git "$EOD2_DIR"
fi

cd "$EOD2_DIR"
git pull --ff-only
git submodule update --init --remote --merge

# Guard on pip, not on the interpreter: `python -m venv` writes .venv/bin/python
# before it bootstraps pip, so the ensurepip failure described above leaves an
# executable interpreter behind with no pip next to it. Keying the guard on
# .venv/bin/python would skip recreation forever and every later run would die
# on the pip calls below. Wipe the directory before rebuilding, and again if the
# rebuild fails, so a half-built venv never survives into the next run.
if [ ! -x .venv/bin/pip ]; then
  rm -rf .venv
  if ! "$EOD2_PYTHON" -m venv .venv || [ ! -x .venv/bin/pip ]; then
    rm -rf .venv
    echo "eod2-sync: $EOD2_PYTHON could not build a venv with a working pip; set EOD2_PYTHON to an interpreter whose ensurepip works" >&2
    exit 1
  fi
fi
.venv/bin/pip install --quiet --upgrade pip
.venv/bin/pip install --quiet -r requirements.txt

# init.py syncs eod2_data up to the latest session; run after 19:00 IST for today's bars.
.venv/bin/python src/init.py

echo "eod2 data ready at $EOD2_DIR/src/eod2_data"
