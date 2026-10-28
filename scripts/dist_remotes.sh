# Sourced by the distribution publish scripts (publish_dist.sh and
# publish_notices.sh); not run on its own.
#
# Loads the publishing credentials when the environment has none (the
# worktree's secrets/dist-publish.env, falling back to the main checkout's
# ../gpuworks/secrets/dist-publish.env), checks the storage keys are set and
# defines the rclone remotes oss:, obs: and r2: from the environment, so no
# config file holds the keys. Sets $bucket, $remotes and $publics.
for env_file in secrets/dist-publish.env ../gpuworks/secrets/dist-publish.env; do
  if [ -z "${GPUWORKS_DIST_R2_ACCESS_KEY_ID:-}" ] && [ -f "$env_file" ]; then
    # shellcheck disable=SC1090
    . "$env_file"
  fi
done
: "${GPUWORKS_DIST_OSS_ACCESS_KEY_ID:?}" "${GPUWORKS_DIST_OSS_SECRET_ACCESS_KEY:?}"
: "${GPUWORKS_DIST_OBS_ACCESS_KEY_ID:?}" "${GPUWORKS_DIST_OBS_SECRET_ACCESS_KEY:?}"
: "${GPUWORKS_DIST_R2_ACCESS_KEY_ID:?}" "${GPUWORKS_DIST_R2_SECRET_ACCESS_KEY:?}" "${GPUWORKS_DIST_R2_ENDPOINT:?}"

bucket=${GPUWORKS_DIST_BUCKET:-gpuworks-dl}

# The publishing keys may only read and write objects in the bucket, so
# rclone must not try to create it before an upload (no_check_bucket).
export RCLONE_CONFIG_OSS_TYPE=s3 RCLONE_CONFIG_OSS_NO_CHECK_BUCKET=true RCLONE_CONFIG_OSS_PROVIDER=Alibaba \
  RCLONE_CONFIG_OSS_ENDPOINT=oss-cn-shanghai.aliyuncs.com RCLONE_CONFIG_OSS_ACL=public-read \
  RCLONE_CONFIG_OSS_ACCESS_KEY_ID="$GPUWORKS_DIST_OSS_ACCESS_KEY_ID" \
  RCLONE_CONFIG_OSS_SECRET_ACCESS_KEY="$GPUWORKS_DIST_OSS_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_OBS_TYPE=s3 RCLONE_CONFIG_OBS_NO_CHECK_BUCKET=true RCLONE_CONFIG_OBS_PROVIDER=HuaweiOBS \
  RCLONE_CONFIG_OBS_ENDPOINT=obs.cn-east-3.myhuaweicloud.com RCLONE_CONFIG_OBS_REGION=cn-east-3 \
  RCLONE_CONFIG_OBS_ACL=public-read \
  RCLONE_CONFIG_OBS_ACCESS_KEY_ID="$GPUWORKS_DIST_OBS_ACCESS_KEY_ID" \
  RCLONE_CONFIG_OBS_SECRET_ACCESS_KEY="$GPUWORKS_DIST_OBS_SECRET_ACCESS_KEY"
export RCLONE_CONFIG_R2_TYPE=s3 RCLONE_CONFIG_R2_NO_CHECK_BUCKET=true RCLONE_CONFIG_R2_PROVIDER=Cloudflare \
  RCLONE_CONFIG_R2_ENDPOINT="$GPUWORKS_DIST_R2_ENDPOINT" \
  RCLONE_CONFIG_R2_ACCESS_KEY_ID="$GPUWORKS_DIST_R2_ACCESS_KEY_ID" \
  RCLONE_CONFIG_R2_SECRET_ACCESS_KEY="$GPUWORKS_DIST_R2_SECRET_ACCESS_KEY"
remotes="oss:$bucket obs:$bucket r2:$bucket"
publics="https://$bucket.oss-cn-shanghai.aliyuncs.com https://$bucket.obs.cn-east-3.myhuaweicloud.com https://dl.gpuworks.ai"
