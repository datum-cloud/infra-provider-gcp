#!/bin/bash
set -euo pipefail
PARAM_NAME="$1"
REGION="$2"
TOKEN=$(curl -sf -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")
ROLE=$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" http://169.254.169.254/latest/meta-data/iam/security-credentials/)
eval "$(curl -sf -H "X-aws-ec2-metadata-token: $TOKEN" "http://169.254.169.254/latest/meta-data/iam/security-credentials/$ROLE" | jq -r '"AWS_ACCESS_KEY_ID=\(.AccessKeyId)
AWS_SECRET_ACCESS_KEY=\(.SecretAccessKey)
AWS_SESSION_TOKEN=\(.Token)"')"

curl -sf \
  --user "$AWS_ACCESS_KEY_ID":"$AWS_SECRET_ACCESS_KEY" \
  -H "x-amz-security-token: $AWS_SESSION_TOKEN" \
  --aws-sigv4 "aws:amz:$REGION:ssm" \
  -X POST -H "Content-Type: application/x-amz-json-1.1" -H "X-Amz-Target: AmazonSSM.GetParameter" \
  -d "{\"Name\":\"$PARAM_NAME\",\"WithDecryption\":true}" \
  "https://ssm.$REGION.amazonaws.com/" |
jq -r '.Parameter.Value|fromjson|to_entries[]|.key as $k|.value|to_entries[]|"/etc/secrets/content/\($k)/\(.key)\u0000\(.value)\u0000"' |
while IFS= read -r -d '' path && IFS= read -r -d '' value; do
  mkdir -p "$(dirname "$path")"
  echo "$value" | base64 -d > "$path"
done
