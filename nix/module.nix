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
  hasToken = cfg.settings ? githubTokenFile;
  credentialPath = "/run/credentials/decky-store-proxy.service/github-token";
  finalSettings =
    if hasToken then cfg.settings // { githubTokenFile = credentialPath; } else cfg.settings;
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
        `upstream`, `topic`, `refreshInterval` or `githubTokenFile` (a path
        to a file holding a GitHub token; passed to the service as a systemd
        credential, so it may be root-only).

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
          orgs = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            default = [ ];
            example = [ "paulmiro-decky-store" ];
            description = ''
              GitHub organizations whose public, non-archived repositories
              tagged with the `decky-plugin` topic are offered as plugins.
            '';
          };
          extraRepos = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            default = [ ];
            example = [ "someone/some-plugin" ];
            description = ''
              Additional repositories (`owner/repo` or GitHub URLs) to offer.
              They must publish releases in the same format. They win over
              organization and upstream plugins of the same name.
            '';
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
        LoadCredential = lib.mkIf hasToken [ "github-token:${toString cfg.settings.githubTokenFile}" ];
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
