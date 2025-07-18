# On chromium sandboxing

## Executive summary on chromium sandboxing inside containers

The mechanisms chromium uses to create its sandbox are the same on which containers work, and more importantly, also the same that [container escape](https://www.aquasec.com/cloud-native-academy/container-security/container-escape/) vulnerabilities rely on. For this reason, container runtimes block this mechanisms inside containers.

To allow chromium to run its sandbox one must relax certain security mechanisms of the container runtime itself, and conversely, to preserve the security benefits of the container runtime one must disable the chromium sandbox. The options that we have are:
1. `seccomp=unconfined`: Put more steam on preventing escapes from the chromium sandbox, knowing if that happens it *might* be easier for jump to the host.
2. `--no-sandbox`: Put more steam on preventing the code from escaping the container, knowing if that happens it *might* be easier to escape from the chromium sandbox to the container (but not to the host).
3. Invest significant resources in security research and maintenance, aiming to produce a seccomp profile that manages to allow the chromium sandbox to work, but still makes harder escaping from the container.

Historically, for synthetic monitoring and this repo, we have picked option 2.

## Summary of sandboxing strategies

Being a web browser, chromium implements a series of security measures to try and isolate individual processes that run external code (javascript). This functionality is on by default, an can be disabled by launching chromium with `--no-sandbox`.

On linux, chromium achieves this isolation by creating new PID and network namespaces to child processes using [clone with `CLONE_NEWPID | CLONE_NEWNET`](https://man7.org/linux/man-pages/man2/clone.2.html). These flags are normally privileged, meaning that unprivileged processes are not allowed to use them. Chromium tries multiple mechanisms to get access to them:

- Relying on user namespaces, if available.
- Relying on running as root (or having `CAP_SYS_ADMIN`)
- Relying on a helper binary with the `setuid` bit set

The first route, which I believe is chromium's preferred one, may be worth elaborating a bit. As it was stated before, `CLONE_NEWPID` and `CLONE_NEWNET` are privileged flags. However, modern kernels allow unprivileged users to use `CLONE_NEWUSER`. `CLONE_NEWUSER` creates a new user namespace, where the process inside can become root, or grant itself very wide capabilities. By becoming privileged inside that particular namespace, that process can now use `CLONE_NEWPID` and `CLONE_NEWNET`.

If the `chromium` process does not have the capability to create user namespaces, it will try to use a helper binary, called `/usr/lib/chromium/chrome-sandbox`. This helper has the setuid bit, so it will run as root regardless of the user who invokes this.

## Chromium sandbox compatibility with containers

| Stategy \ Location | Containerless     | Docker         | Kubernetes      |
|--------------------|-------------------|----------------|-----------------|
| User namespaces    | ️☑️<sup>1</sup>    | ❌<sup>2</sup> | ☑️<sup>3</sup>  |
| Setuid helper      | ✅                | ❌<sup>4</sup> | ☑️<sup>5</sup>  |

[1]: Known not to work in some linux distributions, namely Debian and Ubuntu, forbid the required syscalls for non-root users.

[2]: Known not to work as Docker forbids the required syscalls via [seccomp policies](https://docs.docker.com/engine/security/seccomp/#significant-syscalls-blocked-by-the-default-profile).

[3]: May or may not work depending on host kernel and CRI settings (see 1 and 2). Observed to work if host and CRI allows it (Arch linux, CRI-O).

[4]: Known to not work due to seccomp policies mentioned in [2] also applying, even after you become root through the setuid helper.

[5]: May or may not work depending on CRI settings. Observed to work with CRI-O.

The table above applies to running with the default security options, which can be modified:

| Stategy \ Location | Docker, `seccomp=unconfined` |
|--------------------|------------------------------|
| User namespaces    | ️☑️<sup>1</sup>               |
| Setuid helper      | ✅                           |

[1]: Worked on a GitHub Actions runner, but may not work if the host kernel forbids the required syscall.

* Kubernetes `securityContext.seccompProfile.Unconfined`: TBD
  * Should unblock the user namespaces route provided the host kernel supports them.
  * Should unblock the setuid helper route regardless.

Relaxing the containerization settings is, however, a security tradeoff.


## How chromium decides on a sandboxing strategy

Chromium can sandbox processes on linux in several different ways. Other than itself running with elevated privilages, the two remaning ones are either using helper binary with the `setuid` bit set (`/usr/lib/chromium/chrome-sandbox`), or using user namespaces. There's evidence of this here:
<https://github.com/chromium/chromium/blob/ebb9dbcfdb158f1d45ef1c4ecab2c5a143c90355/sandbox/policy/linux/sandbox_linux.h#L46-L49>

The code that seems to be initializing the sandbox(es) is this function:
<https://github.com/chromium/chromium/blob/ebb9dbcfdb158f1d45ef1c4ecab2c5a143c90355/content/browser/zygote_host/zygote_host_impl_linux.cc#L84>

There is where `--no-sandbox` is handled, for example, and where it checks whether the setuid'd sandbox binary (`/usr/lib/chromium/chrome-sandbox`) is present and sane.

The code below juggles some flags for preferences and some sanity checks and will error as we saw in docker if no sandbox is available and a sandbox was implicitly requested.
Of interest is the call to `CanCreateProcessInNewUserNS()`, which determines whether chromium will fall back to the setuid'd helper or not, as that is checked first.

<https://github.com/chromium/chromium/blob/ebb9dbcfdb158f1d45ef1c4ecab2c5a143c90355/sandbox/linux/services/credentials.cc#L263>

`CanCreateProcessInNewUserNS` checks what the name says by sheer experimentation, doing quite some checks, let's go over them:

1. First, it calls `GetRESIds`. This function essentially fetches the uid and gid of the process by calling `getresuid(2)` and `getresgid(2)` respectively, while also sanity checking that effective and current are equal.
2. Then, it tries to call `ForkWithFlags`, a [wrapper for clone with fork-like behavior](https://github.com/chromium/chromium/blob/ebb9dbcfdb158f1d45ef1c4ecab2c5a143c90355/base/process/launch.h#L432) with [`CLONE_NEWUSER`](https://man7.org/linux/man-pages/man7/user_namespaces.7.html), and returns false if the attempt fails.
3. Checks continue in the child, which the parent monitors and fails if the child itself fails. The child checks the following:
1. It calls `SetGidAndUiddmaps`, as a prerequisite for calling `unshare(2)` later according to a comment. This function essentially writes current uid/gid to `/proc/self/{u,g}id_map`, while also calling `KernelSupportsDenySetgroups` which will fail if `/proc/self/setgroups` exists but `deny` cannot be written to it.
2. It calls `DropAllCapabilities`, which eventually just calls `capset(2)` with an empty list.
3. It attempts to call `unshare(2)` with `CLONE_NEWUSER`.

Once running on a new user namespace, the process is free to perform privileged operations such as `CLONE_NEWNET` and `CLONE_NEWPID`.

3.3 is interesting, as according to the [kernel docs](https://man7.org/linux/man-pages/man7/user_namespaces.7.html)
> A call to clone(2) or unshare(2) with the CLONE_NEWUSER flag
  makes the new child process (for clone(2)) or the caller (for
  unshare(2)) a member of the new user namespace created by the
  call.

In any case, running chromium with `strace` on both docker and k8s reveals the hidden truth:

```Dockerfile
ENTRYPOINT [ "strace", "--", "chromium", "--enable-logging=stderr", "--v=1", "--headless", "--remote-debugging-address=0.0.0.0", "--remote-debugging-port=5222" ]
```

In docker:

```
[...]
# Check of the setuid sandbox exists, as part of the setuid sandbox init. This is done before checking if the sandbox is there.
access("/usr/lib/chromium/chrome-sandbox", F_OK) = -1 ENOENT (No such file or directory)
stat("/proc/self/exe", {st_mode=S_IFREG|0755, st_size=209133120, ...}) = 0
getuid()                                = 6666
# Call to GetRESIds
getresuid([6666], [6666], [6666])       = 0
getresgid([6666], [6666], [6666])       = 0
rt_sigprocmask(SIG_BLOCK, ~[], [], 8)   = 0
# Try to ForkWithFlags
clone(child_stack=0x7ffd0e837588, flags=CLONE_NEWUSER|SIGCHLD) = -1 EPERM (Operation not permitted)
rt_sigprocmask(SIG_SETMASK, [], NULL, 8) = 0
gettid()                                = 9
# Complain.
write(2, "[0530/111418.691871:FATAL:zygote"..., 352[0530/111418.691871:FATAL:zygote_host_impl_linux.cc(126)] No usable sandbox! Update your kernel or see https://chromium.googlesource.com/chromium/src/+/main/docs/linux/suid_sandbox_development.md for more information on developing with the SUID sandbox. If you want to live dangerously and need an immediate workaround, you can try using --no-sandbox.
```

In k8s, however:

```
2024-05-30T11:26:05.289753521Z access("/usr/lib/chromium/chrome-sandbox", F_OK) = -1 ENOENT (No such file or directory)
2024-05-30T11:26:05.289775254Z stat("/proc/self/exe", {st_mode=S_IFREG|0755, st_size=209133120, ...}) = 0
2024-05-30T11:26:05.289790531Z getuid()                                = 6666
2024-05-30T11:26:05.289810913Z getresuid([6666], [6666], [6666])       = 0
2024-05-30T11:26:05.289827857Z getresgid([6666], [6666], [6666])       = 0
2024-05-30T11:26:05.289845350Z rt_sigprocmask(SIG_BLOCK, ~], ], 8)   = 0
# Clone succeeds!
2024-05-30T11:26:05.290460149Z clone(child_stack=0x7ffdbb321468, flags=CLONE_NEWUSER|SIGCHLD) = 13
2024-05-30T11:26:05.290481058Z rt_sigprocmask(SIG_SETMASK, ], NULL, 8) = 0
# Wait for child.
2024-05-30T11:26:05.291189440Z wait4(13, [{WIFEXITED(s) && WEXITSTATUS(s) == 0}], 0, NULL) = 13
# Child exits correctly.
2024-05-30T11:26:05.291200788Z --- SIGCHLD {si_signo=SIGCHLD, si_code=CLD_EXITED, si_pid=13, si_uid=6666, si_status=0, si_utime=0, si_stime=0} ---
```

Both in docker and k8s this should be allowed as per the kernel setting:

```
cat /proc/sys/kernel/unprivileged_userns_clone
1
```

But docker denies somewhere on its own [seccomp policies](https://docs.docker.com/engine/security/seccomp/#significant-syscalls-blocked-by-the-default-profile). CRI-O, and possibly containerd, do not deny this, as reported [here](https://github.com/cgwalters/container-cve-2021-22555/blob/main/README.md#note-criopodman-runtimedefault-policy-vs-docker) ([archive.org](https://web.archive.org/web/20240530113241/https://github.com/cgwalters/container-cve-2021-22555/blob/main/README.md#note-criopodman-runtimedefault-policy-vs-docker)).

This is verified by asserting that this successfully launches chromium, with the setuid'd binary removed:

```console
docker run -ti --rm --cap-drop=all --security-opt seccomp=unconfined localhost:5000/browser:latest
```



As for the setuid helper, we have verified that chromium will _not_ use this helper if it has `CAP_SYS_ADMIN`, by simply removing that file and trying to start chromium with the sandbox enabled. It runs without errors:

```Dockerfile
FROM alpine:3.20.0

RUN adduser --home / --uid 6666 --shell /bin/nologin --disabled-password k6
RUN apk --no-cache add chromium-swiftshader
RUN rm /usr/lib/chromium/chrome-sandbox

USER k6

ENTRYPOINT [ "chromium", "--headless", "--remote-debugging-address=0.0.0.0", "--remote-debugging-port=5222" ]
```

```
# We have run rm /usr/lib/chromium/chrome-sandbox in this image.
18:43:00 ~/Devel/crocochrome $> docker run -ti --rm --cap-add=sys_admin localhost:5000/browser:latest
[0529/164303.395670:ERROR:bus.cc(407)] Failed to connect to the bus: Failed to connect to socket /var/run/dbus/system_bus_socket: No such file or directory
[0529/164303.395966:ERROR:bus.cc(407)] Failed to connect to the bus: Failed to connect to socket /var/run/dbus/system_bus_socket: No such file or directory
[0529/164303.395993:ERROR:bus.cc(407)] Failed to connect to the bus: Failed to connect to socket /var/run/dbus/system_bus_socket: No such file or directory
Fontconfig error: No writable cache directories
[0529/164303.403245:INFO:config_dir_policy_loader.cc(118)] Skipping mandatory platform policies because no policy file was found at: /etc/chromium/policies/managed
[0529/164303.403264:INFO:config_dir_policy_loader.cc(118)] Skipping recommended platform policies because no policy file was found at: /etc/chromium/policies/recommended

DevTools listening on ws://0.0.0.0:5222/devtools/browser/b6f2885e-1f1b-4119-8182-12eea1bae20f
[0529/164303.405298:WARNING:bluez_dbus_manager.cc(248)] Floss manager not present, cannot set Floss enable/disable.
[0529/164303.410270:WARNING:sandbox_linux.cc(420)] InitializeSandbox() called with multiple threads in process gpu-process.
```

If we do not add that specific capability, chromium won't start as user namespaces are not available in docker (more on this later):

```
# We have run rm /usr/lib/chromium/chrome-sandbox in this image.
18:47:58 ~/Devel/crocochrome $> docker run -ti --rm --cap-add=all --cap-drop=sys_admin localhost:5000/browser:latest
[0529/164805.302206:FATAL:zygote_host_impl_linux.cc(126)] No usable sandbox! Update your kernel or see https://chromium.googlesource.com/chromium/src/+/main/docs/linux/suid_sandbox_development.md for more information on developing with the SUID sandbox. If you want to live dangerously and need an immediate workaround, you can try using --no-sandbox.
```

In more recent chromium versions the error instead becomes:
```
[13:13:0708/103542.786751:FATAL:content/browser/zygote_host/zygote_host_impl_linux.cc:132] No usable sandbox! If you are running on Ubuntu 23.10+ or another Linux distro that has disabled unprivileged user namespaces with AppArmor, see https://chromium.googlesource.com/chromium/src/+/main/docs/security/apparmor-userns-restrictions.md. Otherwise see https://chromium.googlesource.com/chromium/src/+/main/docs/linux/suid_sandbox_development.md for more information on developing with the (older) SUID sandbox. If you want to live dangerously and need an immediate workaround, you can try using --no-sandbox.
```

Interestingly, the latter scenario does **not** reproduce in k8s: Chromium is able to start with the sandbox enabled, wihtout the `sys_admin` capability, and with the `/usr/lib/chromium/chrome-sandbox` helper being removed from the image:

```
[0529/173417.131639:ERROR:bus.cc(407)] Failed to connect to the bus: Failed to connect to socket /var/run/dbus/system_bus_socket: No such file or directory
[0529/173417.132032:ERROR:bus.cc(407)] Failed to connect to the bus: Failed to connect to socket /var/run/dbus/system_bus_socket: No such file or directory
[0529/173417.132056:ERROR:bus.cc(407)] Failed to connect to the bus: Failed to connect to socket /var/run/dbus/system_bus_socket: No such file or directory
[0529/173417.133214:ERROR:zygote_host_impl_linux.cc(273)] Failed to adjust OOM score of renderer with pid 29: Permission denied (13)
Fontconfig error: No writable cache directories
Fontconfig error: No writable cache directories
[0529/173417.139017:INFO:config_dir_policy_loader.cc(118)] Skipping mandatory platform policies because no policy file was found at: /etc/chromium/policies/managed
[0529/173417.139027:INFO:config_dir_policy_loader.cc(118)] Skipping recommended platform policies because no policy file was found at: /etc/chromium/policies/recommended

DevTools listening on ws://0.0.0.0:5222/devtools/browser/7bf56a77-f480-415e-996d-b6e1d9861362
[0529/173417.140865:WARNING:bluez_dbus_manager.cc(248)] Floss manager not present, cannot set Floss enable/disable.
[0529/173417.144703:WARNING:sandbox_linux.cc(420)] InitializeSandbox() called with multiple threads in process gpu-process.
```

The logs above are produced by the following pod:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: chromium
  labels:
    app.kubernetes.io/name: chromium
spec:
  securityContext:
    runAsUser: 6666
    runAsGroup: 6666
    fsGroup: 6666
  containers:
    - name: chromium-tip
      image: localhost:5000/browser # Same image as we ran with docker
      imagePullPolicy: Always
      securityContext:
        runAsNonRoot: true
        readOnlyRootFilesystem: true
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["all"]
```

