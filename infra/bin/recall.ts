#!/usr/bin/env node
import * as cdk from 'aws-cdk-lib';
import { RecallStack } from '../lib/recall-stack';

const app = new cdk.App();
new RecallStack(app, 'RecallStack', {
  env: {
    account: process.env.CDK_DEFAULT_ACCOUNT,
    region: process.env.CDK_DEFAULT_REGION,
  },
});
