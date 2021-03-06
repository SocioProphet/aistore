## Distributed Sort

The CLI allows users to manage [AIS DSort](/dsort/README.md) jobs.

### Randomly generate shards

`ais gen-shards --template <value> --fsize <value> --fcount <value>`

Puts randomly generated shards that can be used for dSort testing.

| Flag | Type | Description | Default |
| --- | --- | --- | --- |
| `--ext` | `string` | Extension for shards (either `.tar` or `.tgz`) | `.tar` |
| `--bucket` | `string` | Bucket where shards will be put | `dsort-testing` |
| `--template` | `string` | Template of input shard name | `shard-{0..9}` |
| `--fsize` | `string` | Single file size inside the shard, can end with size suffix (k, MB, GiB, ...) | `1024`  (`1KB`)|
| `--fcount` | `int` | Number of files inside single shard | `5` |
| `--cleanup` | `bool` |When set, the old bucket will be deleted and created again | `false` |
| `--conc` | `int` | Limits number of concurrent `PUT` requests and number of concurrent shards created | `10` |


#### Examples

| Command | Explanation |
| --- | --- |
| `ais gen-shards --fsize 262144 --fcount 100` | Generates 10 shards each containing 100 files of size 256KB and puts them inside `dsort-testing` bucket. Shards will be named: `shard-0.tar`, `shard-1.tar`, ..., `shard-9.tar` |
| `ais gen-shards --ext .tgz --template "super_shard_{000..099}_last" --fsize 262144 --cleanup` | Generates 100 shards each containing 5 files of size 256KB and puts them inside `dsort-testing` bucket. Shards will be compressed and named: `super_shard_000_last.tgz`, `super_shard_001_last.tgz`, ..., `super_shard_099_last.tgz` |

### Start

`ais start dsort JSON_SPECIFICATION`

Starts new dSort job with provided specification. Upon creation, `JOB_ID` of the
job is returned - it can then be used to abort it or retrieve metrics. Following
table describes json keys which can be used in specification.

| Key | Type | Description | Required | Default |
| --- | --- | --- | --- | --- |
| `extension` | `string` | extension of input and output shards (either `.tar`, `.tgz` or `.zip`) | yes | |
| `input_format` | `string` | name template for input shard | yes | |
| `output_format` | `string` | name template for output shard | yes | |
| `bucket` | `string` | bucket where shards objects are stored | yes | |
| `provider` | `string` | Cloud provider (ais or cloudi) | no | `"ais"` |
| `output_bucket` | `string` | bucket where new output shards will be saved | no | same as `bucket` |
| `output_provider` | `string` | determines whether the output bucket is ais or cloud | no | same as `provider` |
| `description` | `string` | description of dsort job | no | `""` |
| `output_shard_size` | `string` | size (in bytes) of the output shard, can be in form of raw numbers `10240` or suffixed `10KB` | yes | |
| `algorithm.kind` | `string` | determines which algorithm should be during dSort job, available are: `"alphanumeric"`, `"shuffle"`, `"content"` | no | `"alphanumeric"` |
| `algorithm.decreasing` | `bool` | determines if the algorithm should sort the records in decreasing or increasing order, used for `kind=alphanumeric` or `kind=content` | no | `false` |
| `algorithm.seed` | `string` | seed provided to random generator, used when `kind=shuffle` | no | `""` - `time.Now()` is used |
| `algorithm.extension` | `string` | content of the file with provided extension will be used as sorting key, used when `kind=content` | yes (only when `kind=content`) |
| `algorithm.format_type` | `string` | format type (`int`, `float` or `string`) describes how the content of the file should be interpreted, used when `kind=content` | yes (only when `kind=content`) |
| `order_file` | `string` | URL to the file containing external key map (it should contain lines in format: `record_key[sep]shard-%d-fmt`) | yes (only when `output_format` not provided) | `""` |
| `order_file_sep` | `string` | separator used for splitting `record_key` and `shard-%d-fmt` in the lines in external key map | no | `\t` (TAB) |
| `max_mem_usage` | `string` | limits the amount of total system memory allocated by both dSort and other running processes. Once and if this threshold is crossed, dSort will continue extracting onto local drives. Can be in format 60% or 10GB | no | same as in `config.sh` |
| `extract_concurrency_limit` | `string` | limits number of concurrent shards extracted per disk | no | same as in `config.sh` |
| `create_concurrency_limit` | `string` | limits number of concurrent shards created per disk | no | same as in `config.sh` |
| `extended_metrics` | `bool` | determines if dsort should collect extended statistics | no | `false` |

#### Examples:
* Starts (alphanumeric) sorting dSort job with extended metrics for shards with names `shard-0.tar`, `shard-1.tar`, ..., `shard-9.tar`. Each of output shards will have at least `10240` bytes and will be named `new-shard-0000.tar`, `new-shard-0001.tar`, ...
```bash
ais start dsort '{
    "extension": ".tar",
    "bucket": "dsort-testing",
    "input_format": "shard-{0..9}",
    "output_format": "new-shard-{0000..1000}",
    "output_shard_size": "10KB",
    "description": "sort shards from 0 to 9",
    "algorithm": {
        "kind": "alphanumeric"
    },
    "extract_concurrency_limit": 3,
    "create_concurrency_limit": 5,
    "extended_metrics": true
}'
```

### Show jobs and job status

`ais show dsort [JOB_ID]`

Retrieves status of the dSort with provided `JOB_ID` which is returned upon creation.
Lists all dSort jobs if the `JOB_ID` argument is omitted.

| Flag | Type | Description | Default |
| --- | --- | --- | --- |
| `--regex` | `string` | Regex for the description of dSort jobs | `""` |
| `--refresh` | `int` | Refreshing rate of the progress bar refresh or metrics refresh (in milliseconds) | `1000` |
| `--verbose, -v` | `bool` | Show detailed metrics | `false` |
| `--log` | `string` | Path to file where the metrics will be saved (does not work with progress bar) | `/tmp/dsort_run.txt` |

#### Examples
| Command | Explanation |
| --- | --- |
| `ais show dsort` | Shows all dSort jobs |
| `ais show dsort --regex "^dsort-(.*)"` | Shows all dSort jobs with descriptions starting with `dsort-` prefix |
| `ais show dsort 5JjIuGemR` | Shows short status description of the dSort job with ID `5JjIuGemR` |
| `ais show dsort 5JjIuGemR -v` | Shows detailed metrics of the dSort job with ID `5JjIuGemR` |
| `ais show dsort 5JjIuGemR --refresh 500` | Creates progress bar for the dSort job with ID `5JjIuGemR` and refreshes it every `500` milliseconds |
| `ais show dsort 5JjIuGemR --refresh 500 -v` |  Returns newly fetched metrics of the dSort job with ID `5JjIuGemR` every `500` milliseconds |
| `ais show dsort 5JjIuGemR --refresh 500 --log "/tmp/dsort_run.txt"` | Saves newly fetched metrics of the dSort job with ID `5JjIuGemR` to `/tmp/dsort_run.txt` file every `500` milliseconds |

### Stop

`ais stop dsort JOB_ID`

Stops the dSort job with given `JOB_ID`.

#### Examples

| Command | Explanation |
| --- | --- |
| `ais stop dsort 5JjIuGemR` | Stops the dSort job with ID `5JjIuGemR` |

### rm

`ais rm dsort JOB_ID`

Removes the finished dSort job with given `JOB_ID` from the job list.

#### Examples
| Command | Explanation |
| --- | --- |
| `ais rm dsort 5JjIuGemR` | Removes the dSort job with ID `5JjIuGemR` from the list of dSort jobs |
