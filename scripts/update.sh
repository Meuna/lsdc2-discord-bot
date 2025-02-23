#!/bin/bash

script_dir=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
src_dir=$script_dir/..

find_lambda() {
    python3 -c "import sys, json; key='FunctionName'; f=next(f[key] for f in json.load(sys.stdin)['Functions'] if '$1' in f[key]); print(f)"
}

lambda_list=$(aws lambda list-functions)
frontend_lambda=$(echo $lambda_list | find_lambda "Lsdc2CdkStack-discordBotFrontend")
backend_lambda=$(echo $lambda_list | find_lambda "Lsdc2CdkStack-discordBotBackend")

aws lambda update-function-code \
    --function-name $frontend_lambda \
    --zip-file fileb://$src_dir/frontend.zip \
    --no-cli-pager \
    > /dev/null

aws lambda update-function-code \
    --function-name $backend_lambda \
    --zip-file fileb://$src_dir/backend.zip \
    --no-cli-pager \
    > /dev/null
