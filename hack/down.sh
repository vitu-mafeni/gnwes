#!/usr/bin/env bash
# Tears down the demo kind cluster entirely.
set -euo pipefail
kind delete cluster --name gnwes-demo
