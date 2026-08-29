"""Shared plumbing for the benchmark harnesses.

Three modules:

    catalogue   loads the same config/*.yaml the Go gateway loads
    simulator   the offline routing engine, kept honest by benchmarks/compare_modes.py
    results     writes results files with a provenance block that cannot be hand-forged
"""
