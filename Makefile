BENCH_DIR ?= $(CURDIR)/bench_results
BENCH_COUNT ?= 10

.PHONY: bench bench-compare bench-clear
bench:
	@BENCH_DIR=$(BENCH_DIR) BENCH_COUNT=$(BENCH_COUNT) ./scripts/bench.sh
bench-compare:
	@BENCH_DIR=$(BENCH_DIR) ./scripts/bench_compare.sh
bench-clear:
	rm $(BENCH_DIR)/*
