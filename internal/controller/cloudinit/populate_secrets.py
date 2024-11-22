#!/usr/bin/env python3

import os
import sys
import requests
import base64
import json

def main(secret_manager_url):
    # Fetch the access token
    metadata_url = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
    headers = {"Metadata-Flavor": "Google"}
    token_response = requests.get(metadata_url, headers=headers)
    access_token = token_response.json().get("access_token")

    # Fetch the secret payload from Secret Manager
    secret_headers = {
        "Authorization": f"Bearer {access_token}",
        "Content-Type": "application/json"
    }
    secrets_response = requests.get(secret_manager_url, headers=secret_headers)

    # Parse and decode the secret payload
    secrets_data = secrets_response.json().get("payload", {}).get("data")
    if secrets_data:
        secrets_json = json.loads(base64.b64decode(secrets_data).decode())

        # Iterate through the top-level keys and values in the secrets JSON
        for key, nested_data in secrets_json.items():
            # Create a directory for each top-level key
            os.makedirs(f"/etc/secrets/content/{key}", exist_ok=True)

            # Iterate over nested child keys and values, decode if necessary, and save each to a file
            for child_key, child_value in nested_data.items():
                decoded_value = base64.b64decode(child_value).decode()  # decode the base64-encoded value
                with open(f"/etc/secrets/content/{key}/{child_key}", "w") as file:
                    file.write(decoded_value)
    else:
        print("Error: No data field found in payload.")

if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("Usage: script.py <secret_manager_url>")
        sys.exit(1)

    secret_manager_url = sys.argv[1]
    main(secret_manager_url)
