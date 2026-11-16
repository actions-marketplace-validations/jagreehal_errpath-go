# Releasing

1. Set `const Version` in `version.go` to `x.y.z`.
2. Tag and push: `git tag vX.Y.Z && git push origin vX.Y.Z`.
3. The release workflow builds platform binaries and attaches them to the GitHub release.
