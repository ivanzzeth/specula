package integrate

// What hosts.toml actually governs — and what it cannot.
//
// A node reported all-green: hosts.toml written for every registry, CRI and
// transfer config_path both /etc/containerd/certs.d, Specula answering 401 on
// /v2/. A consumer on that node still failed to pull, because it called
// containerd.Client.Pull directly. That path constructs its own resolver and
// never reads hosts.toml, so it dialled registry-1.docker.io and timed out —
// which looked like "Specula is not working" when the truth was "this client
// never asked Specula".
//
// Doctor was not wrong; it was answering a narrower question than operators
// heard. Green meant "the paths I check are wired", and nothing said which
// consumers those are. So it now says so, every run:
//
//	governed by certs.d/hosts.toml
//	  - kubelet and crictl                    (CRI registry config_path)
//	  - ctr images pull, and anything using
//	    the transfer service                  (transfer.v1.local config_path)
//	  - ctr --hosts-dir <dir>                 (explicit)
//
//	NOT governed
//	  - containerd.Client.Pull from Go code   (builds its own resolver)
//	  - any client that constructs a
//	    docker.Resolver without hosts config
//
// This is a property of the containerd API, not a misconfiguration, so it is
// reported as advice rather than a risk: a doctor that prints a red line on every
// correctly configured node teaches operators to ignore doctor. The cases an
// operator CAN fix — colon config_path, empty transfer config_path, residual
// server=, wrong certs.d root — stay risks and are checked elsewhere.

// barePullAdvisory returns the standing note about which consumers hosts.toml
// covers. It is emitted on every run, including green ones, because the failure it
// prevents is someone reading "doctor OK" as "every pull on this node goes through
// Specula".
func barePullAdvisory() Result {
	return Result{
		Protocol: "oci",
		Action:   "advice",
		Detail: "certs.d/hosts.toml governs kubelet + crictl (CRI config_path), " +
			"ctr images pull and other transfer-service clients (transfer.v1.local config_path), " +
			"and ctr --hosts-dir. It does NOT govern a bare containerd.Client.Pull from Go: " +
			"that path builds its own resolver, so it bypasses hosts.toml entirely and dials " +
			"the registry directly (registry-1.docker.io), which in CN times out and looks " +
			"like a Specula failure. Fix in the caller: pass a resolver built with " +
			"docker.ConfigureDefaultRegistries(docker.WithHostsDir(\"/etc/containerd/certs.d\")) " +
			"via containerd.WithResolver, or pull through the transfer service " +
			"(client.Transfer with an image-store/registry pair) so config_path applies. " +
			"Doctor cannot detect this from the node — it is a property of the calling " +
			"code, not of the configuration — so check the client, not the node.",
		Path: "containerd hosts.toml scope",
	}
}
