# bin

The programs are built here on demand, one folder per platform. The binaries are not stored in the repository.

```
./compile.sh             # Linux amd64 (default)
./compile.sh -windows    # Windows amd64
./compile.sh -arm64      # Linux arm64
./compile.sh -all        # all platforms
```

| Folder        | Platform       | Build option |
| ------------- | -------------- | ------------ |
| `linux-amd64` | Linux x86-64   | `-linux`     |
| `linux-arm64` | Linux ARM64    | `-arm64`     |
| `win-amd64`   | Windows x86-64 | `-windows`   |

Every folder has its own `.gitignore`: the binaries stay out of the repository, the folder stays in. The build runs in a Go container (see [docs/build.md](../docs/build.md)). The container image is the main
deliverable; these binaries are for running the programs directly.
