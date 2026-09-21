
ticks per ms (median slope, fallback 10000): 9999.6

## Ban lists

| step | outcome | fetch ms | body bytes | entries | parse ms | build ms | 1000 lookups ms | serialize ms | write ms |
|---|---|---|---|---|---|---|---|---|---|

## Reader line limit

| step | line bytes | written | write ms | read back | intact | read ms |
|---|---|---|---|---|---|---|
| line-32k | 32768 | true | 0.4 | true (8191 bytes) | 1 | 6.5 |
| line-48k | 49152 | true | 0.4 | true (8191 bytes) | 1 | 1.4 |
| line-65535 | 65535 | true | 0.3 | true (8191 bytes) | 1 | 2.2 |
| line-65536 | 65536 | true | 0.4 | true (8191 bytes) | 1 | 2.6 |
| line-65537 | 65537 | true | 0.4 | true (8191 bytes) | 1 | 1.3 |
| line-80k | 81920 | true | 0.3 | true (8191 bytes) | 1 | 1.1 |
| line-96k | 98304 | true | 0.3 | true (8191 bytes) | 1 | 1.4 |
| line-128k | 131072 | true | 0.3 | true (8191 bytes) | 1 | 1.1 |

## Raw responses

| step | outcome | fetch ms | body bytes | string length |
|---|---|---|---|---|
| raw-2m | success | 97 | 2097152 | 2097152 |
| raw-4m | success | 150 | 4194304 | 4194304 |
| raw-8m | success | 304 | 8388608 | 8388608 |
| raw-16m | success | 1114 | 16777216 | 16777216 |
| raw-32m | success | 6086 | 33554432 | 33554432 |
