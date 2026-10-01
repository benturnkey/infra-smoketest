{
  description = "Kubernetes infrastructure smoke-test controller and probes";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      eachSystem = nixpkgs.lib.genAttrs systems;
    in
    {
      packages = eachSystem (
        system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          app = pkgs.buildGoModule {
            pname = "infra-smoketest";
            version = "0.1.0";
            src = pkgs.lib.fileset.toSource {
              root = ./.;
              fileset = pkgs.lib.fileset.unions [
                ./go.mod
                ./go.sum
                ./api
                ./cmd
                ./internal
                (pkgs.lib.fileset.difference ./config ./config/terraform)
                ./examples
              ];
            };
            vendorHash = "sha256-BHUbI+ZJuntaSBskf+112RsfMz46MU/+1YSrJvjNaDI=";
            subPackages = [ "cmd/infra-smoketest" ];
            env.CGO_ENABLED = "0";
            ldflags = [
              "-s"
              "-w"
            ];
            checkPhase = ''
              runHook preCheck
              go test ./...
              runHook postCheck
            '';
          };
        in
        {
          default = app;
          # Both controller and probe subcommands use this same image/digest.
          image = pkgs.dockerTools.buildLayeredImage {
            name = "infra-smoketest";
            tag = "dev";
            contents = [
              app
              pkgs.cacert
            ];
            extraCommands = "mkdir -m 1777 tmp";
            config = {
              Entrypoint = [ "/bin/infra-smoketest" ];
              Cmd = [ "controller" ];
              User = "65532:65532";
              Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt" ];
              WorkingDir = "/";
            };
          };
        }
      );

      devShells = eachSystem (
        system:
        let
          pkgs = import nixpkgs {
            inherit system;
            config.allowUnfreePredicate = pkg: nixpkgs.lib.getName pkg == "terraform";
          };
          assets = pkgs.linkFarm "envtest-assets" [
            {
              name = "kube-apiserver";
              path = "${pkgs.kubernetes}/bin/kube-apiserver";
            }
            {
              name = "etcd";
              path = "${pkgs.etcd}/bin/etcd";
            }
            {
              name = "kubectl";
              path = "${pkgs.kubectl}/bin/kubectl";
            }
          ];
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              go
              gopls
              gnumake
              git
              kubernetes-controller-tools
              kubectl
              kustomize
              actionlint
              shellcheck
              nixfmt
              skopeo
              terraform
            ];
            KUBEBUILDER_ASSETS = "${assets}";
            GOTOOLCHAIN = "local";
            shellHook = ''
              export GOCACHE="/tmp/infra-smoketest-cache/go-build"
              export GOPATH="/tmp/infra-smoketest-cache/go"
            '';
          };
        }
      );
      checks = eachSystem (system: {
        build = self.packages.${system}.default;
      });
      formatter = eachSystem (system: nixpkgs.legacyPackages.${system}.nixfmt);
    };
}
