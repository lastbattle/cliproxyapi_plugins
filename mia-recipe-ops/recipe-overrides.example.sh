# Merge these settings into the Mia recipe's existing scripts/local.sh.
# Do not replace that file: it can contain network settings and a private HF_TOKEN.
# Observed profile: two 128 GB DGX Sparks, Mia main 3fd60df, TensorFold v0.6.0.
CONTEXT=786432
PARALLEL=4
MAX_TOKENS=32768
MEMORY_RESERVE_GIB=23
KV_POOL_GIB=4
MAX_QUEUED=6
export TF_GLM_KEPT_BYTES_GIB=2
export TF_GLM_KEEP_PER_CHAT=2
export TF_GLM_EFFORT_TAIL=1
export TF_GLM_LOOP_GUARD=1
export TF_GLM_MULTI_WATCHDOG_S=180
export TF_GLM_MULTI_WATCHDOG_EXIT=1
