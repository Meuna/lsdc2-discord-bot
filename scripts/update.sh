# /bin/bash

find_lambda() {
    python3 -c "import sys, json; key='FunctionName'; f=next(f[key] for f in json.load(sys.stdin)['Functions'] if '$1' in f[key]); print(f)"
}

lambda_list=$(aws lambda list-functions)
frontend_lambda=$(echo $lambda_list | find_lambda "Lsdc2CdkStack-discordBotFrontend")
backend_lambda=$(echo $lambda_list | find_lambda "Lsdc2CdkStack-discordBotBackend")

aws lambda update-function-code \
    --function-name $frontend_lambda \
    --zip-file fileb://$(pwd)/build/frontend.zip \
    --no-cli-pager \
    > /dev/null

aws lambda update-function-code \
    --function-name $backend_lambda \
    --zip-file fileb://$(pwd)/build/backend.zip \
    --no-cli-pager \
    > /dev/null
