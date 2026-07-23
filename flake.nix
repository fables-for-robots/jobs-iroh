{
  description = "jobs-iroh";
  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

    systems.url = "github:nix-systems/default";

  };

  outputs = { self, nixpkgs, systems, ... }@inputs:
    let
      eachSystem = f:
        nixpkgs.lib.genAttrs (import systems)
        (system: f system nixpkgs.legacyPackages.${system});
    in {

      devShells = eachSystem (system: pkgs: {
        default = pkgs.mkShell {
          shellHook = ''
            # Set here the env vars you want to be available in the shell
          '';
          hardeningDisable = [ "all" ];

          packages = with pkgs; [ go gh ];
        };
      });

      # The build/dev sandbox userland: a FULLY STATIC bash + jq + busybox, with
      # no /nix/store references, so the sandbox needs no /nix bind mount (it is
      # relocatable and hermetic). busybox supplies the coreutils/sed/grep/tar
      # applets; bash and jq are real static builds. The hostshell fetcher
      # materializes this package into the shell artifact.
      packages = eachSystem (system: pkgs: {
        muslRuntime =
          # musl names its dynamic loader per-architecture (ld-musl-x86_64.so.1 on
          # amd64, ld-musl-aarch64.so.1 on arm64). Derive the arch token from the
          # build platform so this evaluates on every system, not just x86_64.
          let muslArch = pkgs.stdenv.hostPlatform.parsed.cpu.name;
          in pkgs.runCommand "jobs-musl-runtime" { } ''
          mkdir -p $out
          # ld-musl-${muslArch}.so.1 is the musl dynamic loader (== libc, self-contained).
          # Use the `.out` output explicitly — it carries lib/ (the default
          # `nix build nixpkgs#musl` would pick the `bin` output, which has no lib/).
          # -L dereferences the loader→libc.so symlink so we copy the real object.
          cp -L ${pkgs.musl.out}/lib/ld-musl-${muslArch}.so.1 $out/ld-musl-${muslArch}.so.1
          # The musl-host rustc/cargo binaries (and unwinding proc-macros/build
          # scripts) are NEEDED-linked against libgcc_s.so.1, which the rust tarball
          # does NOT bundle. Ship the musl-built one — relocatable: it depends only
          # on musl libc (no /nix rpath, no glibc), so the planted loader resolves it.
          cp -L ${pkgs.pkgsMusl.gccForLibs.lib}/lib/libgcc_s.so.1 $out/libgcc_s.so.1
        '';
        shell = pkgs.runCommand "jobs-sandbox-shell" { } ''
          mkdir -p $out/bin
          cp -L ${pkgs.pkgsStatic.bash}/bin/bash      $out/bin/bash
          cp -L ${pkgs.pkgsStatic.jq.bin}/bin/jq      $out/bin/jq
          cp -L ${pkgs.pkgsStatic.busybox}/bin/busybox $out/bin/busybox
          chmod +w $out/bin/bash $out/bin/jq $out/bin/busybox
          # coreutils + friends as busybox applet symlinks (relative → relocatable).
          for t in sh cat ls cp ln mkdir rm mv chmod test true false \
                   env printf dirname basename sed grep tar; do
            ln -s busybox $out/bin/$t
          done
        '';
      });
    };
}
