# Guest kernel source offer

The Fabo guest kernel is Linux under GPL-2.0-only. For every Fabo release, the
complete corresponding source for the bundled kernel is attached to that
release on GitHub:

`https://github.com/OpenFabrica/Fabo/releases/download/vVERSION/Fabo-VERSION-kernel-source.tar.xz`

Replace `VERSION` with the Fabo release version. The archive contains the exact
upstream Linux release, all Fabo patches already applied, the kernel
configuration, and the build inputs. It is also reproducible from this checkout
with:

```sh
scripts/ci/kernel-source.sh target/Fabo-VERSION-kernel-source.tar.xz
```

This offer to provide the corresponding source is valid for three years from
the date of each release. The guest kernel is a separate work from the
Apache-2.0 Fabo application.
