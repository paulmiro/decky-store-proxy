{
  lib,
  buildGoModule,
}:

buildGoModule (finalAttrs: {
  pname = "decky-store-proxy";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      (lib.fileset.fileFilter (file: file.hasExt "go") ../.)
    ];
  };

  vendorHash = "sha256-pbA/AlBz3cQYRTMnQ/qBPcinYOKokrBLNhkbRTq54gE=";

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${finalAttrs.version}"
  ];

  meta = {
    description = "Pull-through Decky Loader plugin store that adds plugins from GitHub releases";
    mainProgram = "decky-store-proxy";
    platforms = lib.platforms.linux;
  };
})
