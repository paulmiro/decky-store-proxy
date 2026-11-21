{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.decky-store-proxy;
  settingsFormat = pkgs.formats.toml { };

  # With DynamicUser the service can't read root-owned secrets directly, so a
  # configured token file is passed in as a systemd credential instead.
  githubCfg = cfg.settings.sources.github;
  hasToken = githubCfg ? tokenFile;
  credentialPath = "/run/credentials/decky-store-proxy.service/github-token";
  finalSettings =
    if hasToken then
      lib.recursiveUpdate cfg.settings { sources.github.tokenFile = credentialPath; }
    else
      cfg.settings;
  configFile = settingsFormat.generate "decky-store-proxy.toml" finalSettings;
in
{
  options.services.decky-store-proxy = {
    enable = lib.mkEnableOption "the pull-through Decky Loader plugin store";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./nix/package.nix { }";
      description = "The decky-store-proxy package to use.";
    };

    settings = lib.mkOption {
      default = { };
      description = ''
        Configuration written to the TOML config file. Besides the options
        below, any other key the program understands can be set here, e.g.
        `upstream`, `refreshInterval` or `sources.github.tokenFile`.

        Point Decky (Settings → General → Store Channel → Custom) at
        `http://<listenAddress>:<port>/plugins`.
      '';
      type = lib.types.submodule {
        freeformType = settingsFormat.type;
        options = {
          port = lib.mkOption {
            type = lib.types.port;
            default = 8737;
            description = "TCP port to listen on.";
          };
          listenAddress = lib.mkOption {
            type = lib.types.str;
            default = "127.0.0.1";
            description = "Address to listen on. Only loopback is needed for Decky.";
          };
          sources = lib.mkOption {
            default = { };
            description = "Where custom plugins come from.";
            type = lib.types.submodule {
              freeformType = settingsFormat.type;
              options.github = lib.mkOption {
                default = { };
                description = ''
                  Plugins from GitHub releases (the `[sources.github]` table).
                  Also accepts `tokenFile`: a file holding a GitHub token, only
                  needed beyond 60 API requests/hour. It is passed to the
                  service as a systemd credential, so it may be root-only.
                '';
                type = lib.types.submodule {
                  freeformType = settingsFormat.type;
                  options = {
                    orgs = lib.mkOption {
                      type = lib.types.listOf lib.types.str;
                      default = [ ];
                      example = [ "some-org-name" ];
                      description = ''
                        GitHub organizations whose public, non-archived
                        repositories with one of `topics` are offered.
                      '';
                    };
                    users = lib.mkOption {
                      type = lib.types.listOf lib.types.str;
                      default = [ ];
                      example = [ "paulmiro" ];
                      description = ''
                        GitHub users whose own public, non-archived
                        repositories with one of `topics` are offered.
                      '';
                    };
                    repos = lib.mkOption {
                      type = lib.types.listOf lib.types.str;
                      default = [ ];
                      example = [ "someone/some-plugin" ];
                      description = ''
                        Individual repositories (`owner/repo` or GitHub URLs),
                        offered regardless of topics. They win over org, user
                        and upstream plugins of the same name.
                      '';
                    };
                    topics = lib.mkOption {
                      type = lib.types.nonEmptyListOf lib.types.str;
                      default = [ "decky-plugin" ];
                      description = ''
                        An org or user repository is offered if it has at
                        least one of these GitHub topics.
                      '';
                    };
                  };
                };
              };
            };
          };
        };
      };
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.decky-store-proxy = {
      description = "Pull-through Decky Loader plugin store";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      restartTriggers = [ configFile ];

      serviceConfig = {
        ExecStart = "${lib.getExe cfg.package} -config ${configFile}";
        LoadCredential = lib.mkIf hasToken [ "github-token:${toString githubCfg.tokenFile}" ];
        DynamicUser = true;
        CacheDirectory = "decky-store-proxy";
        Restart = "on-failure";
        RestartSec = 5;

        CapabilityBoundingSet = "";
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = true;
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectProc = "invisible";
        ProtectSystem = "strict";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged"
        ];
        UMask = "0077";
      };
    };
  };
}
