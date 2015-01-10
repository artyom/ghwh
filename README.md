ghwh - github webhook handler
=============================

This small utility can be used to run commands on push github webhook events.

Install it:

	go get -u github.com/artyom/ghwh

Use it:

	Usage of ghwh:
	  -cert="": path to ssl certificate
	  -config="": path to config (yaml)
	  -key="": path to ssl certificate key
	  -listen="127.0.0.1:8080": address to listen at
	  -qsize=10: job queue size

Configuration file example:

```yaml
/hook1:
  reponame: ghwh
  secret: someSecret
  command: /usr/bin/touch
  args:
    - /tmp/ghwh-updated
  refs:
    "refs/heads/dev":
      command: /usr/bin/touch
      args:
        - /tmp/ghwh-dev-updated
        - /tmp/second-arg

/hook2:
  reponame: bar
  refs:
    "refs/heads/master":
      command: /usr/bin/local/some-script
      args:
        - "--branch=master"
```


This configuration defines two hook endpoints for two separate repositories.
First endpoint mapped to `/hook1` and handles hooks for `ghwh` repository,
validating each request against [shared secret][1]. For `refs/heads/dev` ref.
command `/usr/bin/touch /tmp/ghwh-dev-updated /tmp/second-arg` is called, for
every other branch command `/usr/bin/touch /tmp/ghwh-updated`.

Second hook `/hook2` handles updates of `bar` repository and processes only
events for `refs/heads/master` ref., running command
`/usr/bin/local/some-script --branch=master`.

Current implementation runs all commands one by one, queue size can be
configured with `-qsize` flag. This may change in the future.

If both `-cert` and `-key` flags set, ghwh tries to use https protocol,
otherwise plain http is used. If https is used with self-signed certificates,
do not forget to set `insecure_ssl=1` while [setting up webhook][1].

[1]: https://developer.github.com/v3/repos/hooks/#create-a-hook
