import * as path from 'node:path';
import { GoFunction } from '@aws-cdk/aws-lambda-go-alpha';
import * as cdk from 'aws-cdk-lib';
import * as apigwv2 from 'aws-cdk-lib/aws-apigatewayv2';
import { HttpLambdaIntegration } from 'aws-cdk-lib/aws-apigatewayv2-integrations';
import * as dynamodb from 'aws-cdk-lib/aws-dynamodb';
import * as iam from 'aws-cdk-lib/aws-iam';
import * as lambda from 'aws-cdk-lib/aws-lambda';
import { Construct } from 'constructs';

const TOKEN_PARAM = '/recall/api-token';
// OAuth client id is public by design (it ships in the page source).
const GOOGLE_CLIENT_ID =
  '769222284093-ugmcb1112os68pp9fjggc07l0jglj8kg.apps.googleusercontent.com';

export class RecallStack extends cdk.Stack {
  public readonly table: dynamodb.Table;

  constructor(scope: Construct, id: string, props?: cdk.StackProps) {
    super(scope, id, props);

    this.table = new dynamodb.Table(this, 'MainTable', {
      tableName: 'recall-main',
      partitionKey: { name: 'PK', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'SK', type: dynamodb.AttributeType.STRING },
      billingMode: dynamodb.BillingMode.PAY_PER_REQUEST,
      stream: dynamodb.StreamViewType.NEW_AND_OLD_IMAGES,
      timeToLiveAttribute: 'ttl',
      removalPolicy: cdk.RemovalPolicy.RETAIN,
      pointInTimeRecoverySpecification: { pointInTimeRecoveryEnabled: true },
    });

    // Sparse GSI: only reminder notes carry GSI1PK/GSI1SK (due timestamp).
    this.table.addGlobalSecondaryIndex({
      indexName: 'GSI1',
      partitionKey: { name: 'GSI1PK', type: dynamodb.AttributeType.STRING },
      sortKey: { name: 'GSI1SK', type: dynamodb.AttributeType.STRING },
      projectionType: dynamodb.ProjectionType.ALL,
    });

    const ingestFn = new GoFunction(this, 'IngestFn', {
      entry: path.join(__dirname, '..', '..', 'lambdas', 'ingest'),
      architecture: lambda.Architecture.ARM_64,
      memorySize: 256,
      timeout: cdk.Duration.seconds(10),
      environment: {
        TABLE_NAME: this.table.tableName,
        TOKEN_PARAM,
        GOOGLE_CLIENT_ID,
        STATIC_TOKEN_USER: 'joshua',
      },
    });
    // Writes notes; reads idem markers on conditional-put conflicts.
    this.table.grantReadWriteData(ingestFn);
    // The token parameter is created out-of-band (aws ssm put-parameter)
    // so the secret never appears in the CFN template.
    ingestFn.addToRolePolicy(
      new iam.PolicyStatement({
        actions: ['ssm:GetParameter'],
        resources: [
          this.formatArn({
            service: 'ssm',
            resource: 'parameter',
            resourceName: TOKEN_PARAM.replace(/^\//, ''),
            arnFormat: cdk.ArnFormat.SLASH_RESOURCE_NAME,
          }),
        ],
      }),
    );

    const api = new apigwv2.HttpApi(this, 'Api', {
      apiName: 'recall',
      corsPreflight: {
        allowOrigins: ['*'],
        allowMethods: [
          apigwv2.CorsHttpMethod.POST,
          apigwv2.CorsHttpMethod.OPTIONS,
        ],
        allowHeaders: ['Authorization', 'Content-Type', 'Idempotency-Key'],
      },
    });
    const integration = new HttpLambdaIntegration('IngestIntegration', ingestFn);
    api.addRoutes({
      path: '/entries',
      methods: [apigwv2.HttpMethod.POST],
      integration,
    });
    api.addRoutes({
      path: '/',
      methods: [apigwv2.HttpMethod.GET],
      integration,
    });
    api.addRoutes({
      path: '/auth/google',
      methods: [apigwv2.HttpMethod.POST],
      integration,
    });

    // GitHub Actions deploys via OIDC: no long-lived AWS keys in GitHub.
    // The role can only be assumed by main of joshhwuu/recall, and only
    // grants assuming the CDK bootstrap roles that cdk deploy uses.
    const githubOidc = new iam.OpenIdConnectProvider(this, 'GithubOidc', {
      url: 'https://token.actions.githubusercontent.com',
      clientIds: ['sts.amazonaws.com'],
    });
    const deployRole = new iam.Role(this, 'GithubDeployRole', {
      roleName: 'recall-github-deploy',
      assumedBy: new iam.WebIdentityPrincipal(
        githubOidc.openIdConnectProviderArn,
        {
          StringEquals: {
            'token.actions.githubusercontent.com:aud': 'sts.amazonaws.com',
          },
          StringLike: {
            'token.actions.githubusercontent.com:sub':
              'repo:joshhwuu/recall:ref:refs/heads/main',
          },
        },
      ),
    });
    deployRole.addToPolicy(
      new iam.PolicyStatement({
        actions: ['sts:AssumeRole'],
        resources: [`arn:aws:iam::${this.account}:role/cdk-*`],
      }),
    );

    new cdk.CfnOutput(this, 'TableName', { value: this.table.tableName });
    new cdk.CfnOutput(this, 'TableArn', { value: this.table.tableArn });
    new cdk.CfnOutput(this, 'TableStreamArn', {
      value: this.table.tableStreamArn ?? '',
    });
    new cdk.CfnOutput(this, 'ApiUrl', { value: api.apiEndpoint });
  }
}
