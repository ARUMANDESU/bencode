BENCH_DIR ?= $(CURDIR)/bench_results

.PHONY: bench bench-compare
bench:
	@BENCH_DIR=$(BENCH_DIR) ./scripts/bench.sh
bench-compare:
	@BENCH_DIR=$(BENCH_DIR) ./scripts/bench_compare.sh
