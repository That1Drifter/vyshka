
ticks per ms (median slope, fallback 10000): 9998.6

## Ban lists

| step | outcome | fetch ms | body bytes | entries | parse ms | build ms | 1000 lookups ms | serialize ms | write ms |
|---|---|---|---|---|---|---|---|---|---|

## Reader line limit

| step | line bytes | written | write ms | read back | intact | read ms |
|---|---|---|---|---|---|---|
| line-16k | 16384 | true | 0.4 | true (16384 bytes) | 1 | 7.6 |
| line-32k | 32768 | true | 0.5 | true (32768 bytes) | 1 | 6.6 |
| line-48k | 49152 | true | 0.3 | true (49152 bytes) | 1 | 7.6 |
| line-65520 | 65520 | true | 0.5 | true (65520 bytes) | 1 | 8.0 |
| line-65536 | 65536 | true | 0.4 | **server-exited** |  |  |

## Raw responses

| step | outcome | fetch ms | body bytes | string length |
|---|---|---|---|---|
| raw-2m | success | 102 | 2097152 | 2097152 |
| raw-4m | success | 153 | 4194304 | 4194304 |
| raw-8m | success | 309 | 8388608 | 8388608 |
| raw-16m | success | 1123 | 16777216 | 16777216 |
| raw-32m | success | 3996 | 33554432 | 33554432 |
