# On linux capabilities

This project, by nature of using a supervisor _and_ chromium, interfaces strongly with linux capabilities inside kubernetes and/or docker. This is a complex topic and even the maintainer's knowledge of it is not perfect. We try to summarize our findings, assumptions, and the things we have verified below.

The first relevant bit is that the crocochrome supervisor wants to run chromium as a different user. We do this to prevent chromium from reading certain files (while allowing crocochrome to do so), and to prevent the chromium process from interacting with crocochrome altogether, e.g. reading its environment, or sending signals to it. To run chromium as a different user, crocochrome uses Go's ability to specify `syscall.Credential` when launching a process, and it specifies the uid/gid of the nobody user.

Normally, a process running as non-root wouldn't have the ability to do this. To allow crocochrome to do this, we add the following capabilities to its binary:
- `cap_setuid` and `cap_setgid` allow crocochrome to start chromium as another user and group (any user).
- `cap_kill` allows crocochrome to kill chromium, which runs as a different user, when a session ends.
- `cap_chown` allows crocochrome to hand the per-session temporary directory over to the user chromium runs as.
- `cap_dac_override` and `cap_fowner` allow crocochrome to remove the per-session temporary directory once chromium exits. The directory and everything chromium creates inside it belong to another user, and may have permissions that would otherwise prevent crocochrome from deleting them.

In order for this to be effective, we also need to add those capabilities to the "bounding set". In kubernetes, this is done through the container's `securityContext`. These two different places where we specify capabilities (the binary and the container `securityContext` interact in the following ways:
- If the capabilities are not specified anywhere, crocochrome will return an error while trying to run chromium as a different user: It has no permissions.
- If the capabilities are set in the securityContext but not in the binary, the same thing happens. Capabilities added to the securityContext (bounding set) are not granted automatically to any binary inside the container.
- If the capabilities are set in the binary, but not in the securityContext, the CRI will refuse to even start crocochrome, as it has capabilities that are disallowed in the bounding set.
- If capabilities are set both in the binary and in the securityContext, crocochrome will start and will be able to start, kill and clean up after processes that run as different users.

A third variable is `allowPrivilegeEscalation`. As per the [k8s docs](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/), this enforces the `no_new_privs` flag. This flags is defined in the [linux docs](https://www.kernel.org/doc/Documentation/prctl/no_new_privs.txt), but the relevant part is the following one:
> With no_new_privs set [...] file capabilities will not add to the permitted set [...]

This means that setting `allowPrivilegeEscalation: false` will effectively result in the same scenario as if we never added the capabilities to our binary (scenario #2 in the list above). For this reason, crocochrome requires `allowPrivilegeEscalation: true`. To reduce the attack surface this opens, the container image removes every binary with the setuid bit set.

The recommended (container) `securityContext` for crocochrome is the following:
```yaml
securityContext:
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: true # Required for the file capabilities on the crocochrome binary to take effect.
  capabilities:
    # For running chromium as a different user, killing it, and cleaning up its files.
    add: ["setuid", "setgid", "kill", "chown", "dac_override", "fowner"]
    drop: ["all"]
```
