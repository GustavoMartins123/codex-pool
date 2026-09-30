# Analytics query measurements

Measured on September 29, 2026, using Go 1.26.8 on an AMD Ryzen 7 5700X
(16 logical CPUs). Windows used UCRT64 GCC 15.2.0-14; Linux ran in the isolated
Docker verification image. The two benchmark runs were sequential.

`BenchmarkSignalAnalytics` creates disposable SQLite and Bolt databases. Each
scenario contains 365 historical days, three models per account, and 100 live
requests per account. Of the 10 or 100 historical accounts, 80% remain in the
current pool. This produces 10,950 or 109,500 daily cost rows and 1,000 or 10,000
live cost rows. The usage store contains six weeks of origin aggregates and
336 populated hourly aggregates. The fixture checks hourly coverage and the
economics timeline, including exclusion of removed accounts from API value.

The table reports median time per operation from three samples with a one-second
measurement interval. Fixture construction is outside the timed loops. The
handler measurement includes the queries, economics calculation, and JSON
serialization; it excludes authentication, network latency, concurrent writes,
and provider calls. Its cached quota snapshot is empty. These are synthetic,
warm-database measurements, not production latency percentiles.

| Operation | Windows, 10 accounts | Windows, 100 accounts | Linux/Docker, 10 accounts | Linux/Docker, 100 accounts |
| --- | ---: | ---: | ---: | ---: |
| Account daily costs | 9.46 ms | 120.18 ms | 11.28 ms | 130.02 ms |
| Account totals and first dates | 5.80 ms | 82.07 ms | 7.45 ms | 89.08 ms |
| Model daily usage | 2.94 ms | 32.09 ms | 3.61 ms | 34.31 ms |
| Weekly origin usage | 0.15 ms | 1.50 ms | 0.14 ms | 1.48 ms |
| Hourly usage | 0.69 ms | 0.71 ms | 0.69 ms | 0.70 ms |
| Economics, including its two queries | 16.24 ms | 207.83 ms | 19.44 ms | 231.17 ms |
| Complete handler | 20.16 ms | 242.77 ms | 24.28 ms | 266.12 ms |

The economics row includes account daily costs and account totals; these rows
must not be added together when estimating handler cost.

| Handler allocation per operation | 10 accounts | 100 accounts |
| --- | ---: | ---: |
| Windows | 2.47 MiB | 21.66 MiB |
| Linux/Docker | 2.34 MiB | 21.30 MiB |

These allocation figures count all bytes allocated during one operation, not
retained heap or process memory. In the larger scenario, economics accounts for
about 86-87% of elapsed handler time and 90% of allocated bytes. Weekly and hourly
reads are already bounded by their materialized chart rows.

## Recommended optimization

Focus on `buildSignalEconomics` and its SQLite queries. Every refresh currently
reads daily history for all accounts, separately scans that history for totals
and first dates, and then filters removed accounts in Go. Economics only consumes
the first date from the totals query. Model daily usage already restricts its
historical date range, while economics intentionally covers the entire lifetime.

The next change should filter the current account IDs in SQL, aggregate API value
at the date/provider chart grain, and avoid the redundant all-time cost scan.
Preserve each account's first measurement timestamp and subscription billing
boundaries, the lifetime timeline, and the current account membership rule.
Validate removals, re-additions, today's live requests, and UTC day boundaries;
query failures must remain explicit errors. Compare the same benchmark before
and after that change. An external Docker cache service is not justified by
these measurements: the dominant work is local SQLite aggregation and row
decoding.

## Reproduce

Build the embedded frontend first, then use the canonical Windows compiler or
run in the verification image:

```sh
go test -run '^$' -bench '^BenchmarkSignalAnalytics$' -benchmem -benchtime=1s -count=3 .
```

Run without `-race` when comparing performance; use race tests separately for
correctness. Raw local measurement logs are stored under `tmp/verification`.
