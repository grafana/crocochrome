# `choom`

`choom` is a portable, simplified, not compatible version of [`chroom(1)`](https://man7.org/linux/man-pages/man1/choom.1.html), which allows to change a process' `oom_score_adj` to make it more or less attractive to the kernel's OOM killer.

This tiny program exists because binaries need a specific capability to lower OOM scores, `cap_sys_resource`. This capability, however, is not granted in the [default set docker uses](https://github.com/moby/moby/blob/master/oci/caps/defaults.go#L6-L19). Due to how linux capabilities work, binaries with a given capability added will fail to start if the container does not have that capability added as well. In practice, what this means is that if we granted `cap_sys_resource` to the main binary of the container, and attempted to run the container naively with `docker run crocochrome:latest`, the container would cryptically fail to start.

This is a UX problem, where users unfamiliar with the codebase would need to troubleshoot a cryptic error message, but also a problem for testing, where developers would need to go out of the "standard route" to figure out how to add specific capabilities to local kubernetes clusters, or testcontainers, in order to perform tests to the container.

Instead of that, using a tiny helper with the capability added, and calling this helper from the main binary, allows the oom score adjust process to fail gracefully. If the container does not have the required `sys_resource` capability, the OOM score will not be adjusted and a log will be errored:

```
{"time":"2025-01-30T12:50:37.771068928Z","level":"ERROR","msg":"Error changing OOM score. Assuming this is a test environment and continuing anyway.","err":"fork/exec /usr/local/bin/choom: operation not permitted"}
```

But execution will continue, and as OOM score adjusting is not critical to functionality whatsoever, tests will do their job just fine. In production, we grant the container the `sys_resource` capability to handle low-memory situations better.
