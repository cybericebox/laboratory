# shellcheck shell=bash
# Sourced by the image tasks (mise-tasks/image/*, mise-tasks/kind-demux/e2e*): the five images and their references.
#   BUILD_TAG        the tag of every image not named below (default: the time of the run, YYYYmmdd-HHMMSS)
#   <NAME>_IMG       the full reference of one image: CONTROLLER_IMG, AGENT_IMG, PROXY_IMG, NODE_IMG, LAB_IMG
#                    (default cybericebox/laboratory-<name>:$BUILD_TAG)
#   CONTAINER_TOOL   docker (default) or another tool with the same build and push commands
# The component name is also the target of the Dockerfile.
BUILD_TAG="${BUILD_TAG:-$(date +%Y%m%d-%H%M%S)}"
CONTAINER_TOOL="${CONTAINER_TOOL:-docker}"
ALL_IMAGES=(controller agent proxy node lab)

# image_ref NAME: the reference of the image of one component; fails on an unknown name.
image_ref() {
  local name=$1 var
  case " ${ALL_IMAGES[*]} " in
    *" $name "*) ;;
    *) echo "unknown image '$name' (one of: ${ALL_IMAGES[*]})" >&2; return 1 ;;
  esac
  var=$(printf '%s' "$name" | tr '[:lower:]' '[:upper:]')_IMG
  printf '%s\n' "${!var:-cybericebox/laboratory-$name:$BUILD_TAG}"
}
