# /bin/bash
aws lambda update-function-code \
    --function-name $FRONTEND_LAMBDA \
    --zip-file fileb://$(pwd)/frontend.zip \
    --no-cli-pager \
    > /dev/null

aws lambda update-function-code \
    --function-name $BACKEND_LAMBDA \
    --zip-file fileb://$(pwd)/backend.zip \
    --no-cli-pager \
    > /dev/null
