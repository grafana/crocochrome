# On linux capabilities

This project, by nature of using a supervisor _and_ chromium, interfaces strongly with linux capabilities inside kubernetes and/or docker. This is a complex topic and even the maintainer's knowledge of it is not perfect. We try to summarize our findings, assumptions, and the things we have verified below.

The first relevant bit is that the crocochrome supervisor wants to run chromium as a different user. We do this to prevent chromium from reading certain files (while allowing crocochrome to do so), and to prevent the chromium process from interacting with crocochrome altogether, e.g. reading its environment, or sending signals to it. To run chromium as a different user, crocochrome uses Go's ability to specify `syscall.Credential` when launching a process, and it specifies the uid/gid of the nobody user.

Normally, a process running as non-root wouldn't have the ability to do this. To allow crocochrome to do this, we add the `cap_setuid`, `cap_setgid` and `cap_kill` capabilities, with the first two allowing the binary to start another process as another user (any user), and the third allowing it to kill any other process. In order for this to be effective, we also need to add those three capabilities to the "bounding set". In kubernetes, this is done through the container's `securityContext`. These two different places where we specify capabilities (the binary and the container `securityContext` interact in the following ways:
- If the capabilities are not specified anywhere, crocochrome will return an error while trying to run chromium as a different user: It has no permissions.
- If the capabilities are set in the securityContext but not in the binary, the same thing happens. Capabilities added to the securityContext (bounding set) are not granted automatically to any binary inside the container.
- If the capabilities are set in the binary, but not in the securityContext, the CRI will refuse to even start crocochrome, as it has capabilities that are disallowed in the bounding set.
- If capabilities are set both in the binary and in the securityContext, crocochrome will start and will be able to start and kill processes that run as different users.

A third variable is `allowPrivilegeEscalation`. As per the [k8s docs](https://kubernetes.io/docs/tasks/configure-pod-container/security-context/), this enforces the `no_new_privs` flag. This flags is defined in the [linux docs](https://www.kernel.org/doc/Documentation/prctl/no_new_privs.txt), but the relevant part is the following one:
> With no_new_privs set [...] file capabilities will not add to the permitted set [...]

This means that setting `allowPrivilegeEscalation: false` will effectively result in the same scenario as if we never added the capabilities to our binary (scenario #2 in the list above).

The recommended (container) `securityContext` for crocochrome is the following:
```yaml
securityContext:
  runAsNonRoot: true
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    add: ["setuid", "setgid", "kill"] # For dropping privileges and killing children.
    drop: ["all"]
```
