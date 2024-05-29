# <img alt="Crocochrome logo, a mashup between the crocodile emoji and the chromium logo" src="logo.svg" style="height: 1em;" /> Crocochrome

Crocochrome is a chromium supervisor, which runs and reaps chromium processes on demand.

Crocochrome needs to be granted certain linux capabilities to funciton, see [docs/capabilities.md](/doc/capabilities.md) for details.

Crocochrome runs chromium with `--no-sandbox`. The reason for this is that to run with sandboxing enabled, [chromium needs user namespaces to work](doc/chromium-sandbox.md), which are not available everywhere.

Moreover, chromium's sandbox focuses on isolating the processes running untrusted code from other processes, the network, and the filesystem.
Regarding process isolation, we only run one chromium process concurrently, and that's the only process in the container running as the (unprivileged) container. Therefore we do not see much value in this isolation.
Regarding filesystem access, the whole container is run with a read-only filesystem. The Crocochrome binary is not readable or runnable by the user chromium is running on, and there should be no sensitive files to be accessed whatsoever.
Regarding the network, we can use `NetworkPolicy` objects to forbid the Crocochrome container from accessing private IP ranges.
