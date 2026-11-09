//! Linux-only qualification fixture, never installed by the ordinary helper build.
use anyhow::{Context, Result, ensure};
use std::io::Write;
use std::os::unix::process::CommandExt;
use std::process::Command;

const METER_UID: u32 = 60_001;

fn main() {
    if let Err(error) = run() {
        eprintln!("fixture restriction failed: {error:#}");
        std::process::exit(125);
    }
}

fn run() -> Result<()> {
    ensure!(
        std::env::vars_os().next().is_none(),
        "environment is not empty"
    );
    ensure!(
        unsafe { libc::getuid() } == METER_UID && unsafe { libc::geteuid() } == METER_UID,
        "wrong meter identity"
    );
    ensure!(
        unsafe { libc::getgid() } == METER_UID && unsafe { libc::getegid() } == METER_UID,
        "wrong meter group"
    );
    ensure!(
        unsafe { libc::getgroups(0, std::ptr::null_mut()) } == 0,
        "supplementary groups present"
    );
    let status = std::fs::read_to_string("/proc/self/status")?;
    for key in ["CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"] {
        let value = status
            .lines()
            .find_map(|line| line.strip_prefix(&format!("{key}:")))
            .context("missing capability state")?;
        ensure!(
            u64::from_str_radix(value.trim(), 16)? == 0,
            "capabilities remain"
        );
    }
    ensure!(
        unsafe { libc::prctl(libc::PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0) } == 1,
        "no_new_privs is absent"
    );
    // Supervisor supplies /dev/null and bounded pipes, never an SSH socket.
    for fd in 0..=2 {
        let mut stat: libc::stat = unsafe { std::mem::zeroed() };
        ensure!(
            unsafe { libc::fstat(fd, &mut stat) } == 0,
            "stdio unavailable"
        );
        let expected = if fd == 0 {
            libc::S_IFCHR
        } else {
            libc::S_IFIFO
        };
        ensure!(
            stat.st_mode & libc::S_IFMT == expected,
            "unexpected stdio descriptor"
        );
    }
    ensure!(
        unsafe { libc::fcntl(42, libc::F_GETFD) } >= 0,
        "fixture did not seed its inherited descriptor"
    );
    ensure!(
        unsafe { libc::syscall(libc::SYS_close_range, 3u32, u32::MAX, 0u32) } == 0,
        "descriptor closure unavailable"
    );
    install_filter()?;
    let args: Vec<_> = std::env::args().skip(1).collect();
    if args.first().map(String::as_str) == Some("--probe") {
        return probe(args.get(1).context("missing probe")?);
    }
    ensure!(
        args.first().map(String::as_str) == Some("--"),
        "missing helper separator"
    );
    let _ = std::io::stderr().write_all(b"fixture: helper-exec\n");
    let error = Command::new("/usr/local/bin/crabbox-wasmtime-meter")
        .args(&args[1..])
        .env_clear()
        .exec();
    Err(error.into())
}

fn instruction(code: u16, jt: u8, jf: u8, k: u32) -> libc::sock_filter {
    libc::sock_filter { code, jt, jf, k }
}

fn install_filter() -> Result<()> {
    // Native ABI only; new/unknown syscalls fail closed. In particular there is
    // no socket, socketcall, io_uring, ptrace or pidfd_getfd entry in this list.
    #[cfg(target_arch = "x86_64")]
    let arch = 0xc000_003e;
    #[cfg(target_arch = "aarch64")]
    let arch = 0xc000_00b7;
    let mut filter = vec![
        instruction(0x20, 0, 0, 4), // seccomp_data.arch
        instruction(0x15, 1, 0, arch),
        instruction(0x06, 0, 0, libc::SECCOMP_RET_KILL_PROCESS),
        instruction(0x20, 0, 0, 0), // seccomp_data.nr
    ];
    #[cfg(target_arch = "x86_64")]
    filter.extend([
        instruction(0x45, 0, 1, 0x4000_0000), // reject x32
        instruction(0x06, 0, 0, libc::SECCOMP_RET_KILL_PROCESS),
    ]);
    let mut allowed = vec![
        libc::SYS_read,
        libc::SYS_write,
        libc::SYS_readv,
        libc::SYS_writev,
        libc::SYS_pread64,
        libc::SYS_pwrite64,
        libc::SYS_openat,
        libc::SYS_close,
        libc::SYS_fstat,
        libc::SYS_newfstatat,
        libc::SYS_statx,
        libc::SYS_lseek,
        libc::SYS_readlinkat,
        libc::SYS_getdents64,
        libc::SYS_faccessat,
        libc::SYS_faccessat2,
        libc::SYS_fcntl,
        libc::SYS_ioctl,
        libc::SYS_dup,
        libc::SYS_dup3,
        libc::SYS_unlinkat,
        libc::SYS_renameat,
        libc::SYS_renameat2,
        libc::SYS_linkat,
        libc::SYS_fsync,
        libc::SYS_fdatasync,
        libc::SYS_ftruncate,
        libc::SYS_mmap,
        libc::SYS_mprotect,
        libc::SYS_munmap,
        libc::SYS_madvise,
        libc::SYS_brk,
        libc::SYS_mremap,
        libc::SYS_membarrier,
        libc::SYS_rt_sigaction,
        libc::SYS_rt_sigprocmask,
        libc::SYS_rt_sigreturn,
        libc::SYS_sigaltstack,
        libc::SYS_set_tid_address,
        libc::SYS_set_robust_list,
        libc::SYS_futex,
        libc::SYS_rseq,
        libc::SYS_tgkill,
        libc::SYS_sched_yield,
        libc::SYS_sched_getaffinity,
        libc::SYS_clock_gettime,
        libc::SYS_clock_getres,
        libc::SYS_clock_nanosleep,
        libc::SYS_nanosleep,
        libc::SYS_getrandom,
        libc::SYS_getpid,
        libc::SYS_getppid,
        libc::SYS_gettid,
        libc::SYS_getuid,
        libc::SYS_geteuid,
        libc::SYS_getgid,
        libc::SYS_getegid,
        libc::SYS_getgroups,
        libc::SYS_prlimit64,
        libc::SYS_getrlimit,
        libc::SYS_uname,
        libc::SYS_sysinfo,
        libc::SYS_prctl,
        libc::SYS_capget,
        libc::SYS_execve,
        libc::SYS_epoll_create1,
        libc::SYS_epoll_ctl,
        libc::SYS_epoll_pwait,
        libc::SYS_eventfd2,
        libc::SYS_pipe2,
        libc::SYS_ppoll,
        libc::SYS_exit,
        libc::SYS_exit_group,
    ];
    #[cfg(target_arch = "x86_64")]
    allowed.extend([
        libc::SYS_arch_prctl,
        libc::SYS_stat,
        libc::SYS_lstat,
        libc::SYS_readlink,
        libc::SYS_access,
        libc::SYS_open,
        libc::SYS_unlink,
        libc::SYS_rename,
        libc::SYS_link,
        libc::SYS_dup2,
        libc::SYS_epoll_wait,
        libc::SYS_poll,
    ]);
    // libc may probe clone3 then fall back to clone. Only native threads may
    // be cloned; a helper cannot spawn an untracked process with fork semantics.
    filter.extend([
        instruction(0x15, 0, 1, libc::SYS_clone3 as u32),
        instruction(0x06, 0, 0, libc::SECCOMP_RET_ERRNO | libc::ENOSYS as u32),
        instruction(0x15, 0, 4, libc::SYS_clone as u32),
        instruction(0x20, 0, 0, 16), // low 32 bits of args[0]
        instruction(0x45, 0, 1, libc::CLONE_THREAD as u32),
        instruction(0x06, 0, 0, libc::SECCOMP_RET_ALLOW),
        instruction(0x06, 0, 0, libc::SECCOMP_RET_ERRNO | libc::EPERM as u32),
    ]);
    for call in allowed {
        filter.push(instruction(0x15, 0, 1, call as u32));
        filter.push(instruction(0x06, 0, 0, libc::SECCOMP_RET_ALLOW));
    }
    filter.push(instruction(
        0x06,
        0,
        0,
        libc::SECCOMP_RET_ERRNO | libc::EPERM as u32,
    ));
    let program = libc::sock_fprog {
        len: filter.len().try_into()?,
        filter: filter.as_mut_ptr(),
    };
    ensure!(
        unsafe {
            libc::syscall(
                libc::SYS_seccomp,
                libc::SECCOMP_SET_MODE_FILTER,
                0,
                &program,
            )
        } == 0,
        "seccomp installation failed"
    );
    ensure!(
        unsafe { libc::prctl(libc::PR_GET_SECCOMP, 0, 0, 0, 0) } == 2,
        "filter mode absent"
    );
    Ok(())
}

fn denied(result: libc::c_long) -> Result<()> {
    ensure!(
        result == -1 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EPERM),
        "network/privilege probe was not denied"
    );
    Ok(())
}

fn probe(name: &str) -> Result<()> {
    match name {
        "sockets" => {
            for family in [libc::AF_INET, libc::AF_INET6, libc::AF_UNIX] {
                denied(unsafe { libc::syscall(libc::SYS_socket, family, libc::SOCK_STREAM, 0) })?;
            }
            denied(unsafe {
                libc::syscall(
                    libc::SYS_socketpair,
                    libc::AF_UNIX,
                    libc::SOCK_STREAM,
                    0,
                    std::ptr::null_mut::<i32>(),
                )
            })?;
            denied(unsafe {
                libc::syscall(libc::SYS_io_uring_setup, 1, std::ptr::null_mut::<u8>())
            })?;
            denied(unsafe { libc::syscall(libc::SYS_pidfd_getfd, -1, 0, 0) })?;
        }
        "identity" => {
            denied(unsafe { libc::syscall(libc::SYS_setuid, 0) })?;
            ensure!(
                unsafe { libc::fcntl(42, libc::F_GETFD) } == -1,
                "seeded descriptor survived"
            );
            ensure!(
                std::fs::read("/root/controller-sentinel").is_err(),
                "controller sentinel readable"
            );
            ensure!(
                std::fs::read("/run/fixture-key-sentinel").is_err(),
                "private-key sentinel readable"
            );
            ensure!(std::env::vars_os().next().is_none(), "environment appeared");
        }
        "resource" => {
            let mut limit: libc::rlimit = unsafe { std::mem::zeroed() };
            ensure!(
                unsafe { libc::getrlimit(libc::RLIMIT_NPROC, &mut limit) } == 0
                    && limit.rlim_max <= 64,
                "process limit missing"
            );
            ensure!(
                unsafe { libc::getrlimit(libc::RLIMIT_FSIZE, &mut limit) } == 0
                    && limit.rlim_max <= 1_048_576,
                "file limit missing"
            );
        }
        "flood" => loop {
            use std::io::Write;
            std::io::stdout().write_all(&[b'x'; 4096])?;
        },
        "wait" => loop {
            std::thread::sleep(std::time::Duration::from_secs(1));
        },
        "file-limit" => {
            use std::io::Write;
            let mut file = std::fs::OpenOptions::new()
                .write(true)
                .create_new(true)
                .open("/tmp/meter-limit-probe")?;
            file.write_all(&vec![0; 2 * 1024 * 1024])?;
            anyhow::bail!("file-size restriction did not stop the write");
        }
        "sudo" => {
            let error = Command::new("/usr/bin/sudo")
                .args(["-n", "/usr/bin/id", "-u"])
                .env_clear()
                .exec();
            return Err(error.into());
        }
        _ => anyhow::bail!("unknown probe"),
    }
    println!("probe-ok:{name}");
    Ok(())
}
