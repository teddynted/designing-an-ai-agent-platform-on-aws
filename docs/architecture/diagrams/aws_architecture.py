"""
AI Agent Platform on AWS — Milestone 1 architecture, rendered with the official AWS icon set.

Generates: aws-architecture.png

Requires Graphviz (the `dot` binary) and the `diagrams` package:

    brew install graphviz
    python3 -m venv .venv && .venv/bin/pip install diagrams
    .venv/bin/python docs/architecture/diagrams/aws_architecture.py

This is the presentation artifact. `aws-architecture.svg` is the hand-authored
equivalent that renders inline on GitHub. The two must be kept in sync — see
README.md in this directory.

The architecture it depicts is specified in docs/architecture/. Where this
diagram and those documents disagree, the documents win.
"""

from diagrams import Cluster, Diagram, Edge
from diagrams.aws.compute import EC2, Fargate, Lambda
from diagrams.aws.database import RDS, ElastiCache
from diagrams.aws.integration import Eventbridge, SQS
from diagrams.aws.management import (
    Cloudformation,
    Cloudwatch,
    SystemsManagerParameterStore,
)
from diagrams.aws.ml import Bedrock
from diagrams.aws.network import ELB, Endpoint, NATGateway
from diagrams.aws.security import IAM, SecretsManager
from diagrams.aws.storage import S3, EFS
from diagrams.generic.device import Mobile

GRAPH_ATTR = {
    "fontsize": "16",
    "labelloc": "t",
    "pad": "0.6",
    "nodesep": "0.6",
    "ranksep": "1.1",
    "splines": "ortho",
}

# Spot fleets and the singleton are the two things a reader must not miss.
SPOT = Edge(color="#ED7100")
DASHED = Edge(color="#8B5CF6", style="dashed")


def build() -> None:
    with Diagram(
        "AI Agent Platform on AWS — Milestone 1",
        filename="docs/architecture/diagrams/aws-architecture",
        outformat="png",
        show=False,
        direction="LR",
        graph_attr=GRAPH_ATTR,
    ):
        chat = Mobile("Chat channels\n(Slack · WhatsApp)")
        callers = Mobile("Webhook / API callers")

        with Cluster("AWS Cloud — single region, multi-AZ"):

            with Cluster("Control plane (serverless)"):
                bus = Eventbridge("EventBridge")
                reactors = Lambda("scaler · spot-drain\nrouter · metering")
                queue = SQS("SQS\nbuffers GPU cold start")
                bus >> reactors

            with Cluster("VPC 10.0.0.0/16"):

                with Cluster("Public subnets"):
                    alb = ELB("Application LB")
                    nat = NATGateway("NAT Gateway\n(per AZ)")

                with Cluster("Private-app — agent plane"):
                    n8n_main = EC2("n8n main\nOn-Demand")
                    n8n_workers = EC2("n8n workers\nSPOT · stateless")
                    gateway = EC2("OpenClaw Gateway\nOn-Demand · singleton\n0 ingress")
                    sandbox = EC2("Tool sandbox\nno creds · no IMDS")

                with Cluster("Private-app — inference plane"):
                    model_gw = Fargate("Model Gateway\nOpenAI-compatible seam")
                    ollama = EC2("Ollama GPU\nSPOT · scale-to-zero")

                with Cluster("Private-data — no internet route"):
                    rds = RDS("RDS PostgreSQL\nMulti-AZ")
                    redis = ElastiCache("ElastiCache Redis")
                    efs = EFS("EFS\nGateway state · regional")

                with Cluster("VPC endpoints"):
                    ep_bedrock = Endpoint("bedrock-runtime")
                    ep_s3 = Endpoint("S3 · Gateway EP\nfree")

            with Cluster("Managed — no capacity to run"):
                bedrock = Bedrock("Amazon Bedrock\ndefault + backstop")
                s3 = S3("S3\nweights · artifacts")
                cw = Cloudwatch("CloudWatch")
                secrets = SecretsManager("Secrets Manager")
                ssm = SystemsManagerParameterStore("SSM Parameter Store")
                cfn = Cloudformation("CloudFormation")
                iam = IAM("IAM")

        # Ingress: the webhook path terminates at the ALB; the chat path does not.
        callers >> alb >> n8n_main
        # The Gateway dials out, so it needs no inbound ingress at all.
        chat << DASHED << gateway

        # Workflow execution
        n8n_main >> redis >> n8n_workers
        n8n_main >> rds
        n8n_workers >> rds

        # Agent execution
        gateway >> sandbox
        gateway >> efs

        # Everything reaches a model through one seam.
        n8n_workers >> model_gw
        gateway >> model_gw
        model_gw >> ep_bedrock >> bedrock
        model_gw >> SPOT >> ollama

        # Bedrock is the backstop when Spot GPU capacity is unavailable.
        model_gw >> Edge(color="#DD344C", style="dashed", label="fallback") >> bedrock

        # Scale the GPU fleet from zero on queue depth.
        queue >> reactors
        reactors >> SPOT >> ollama

        # Weights come from S3 over the free Gateway Endpoint, never the NAT.
        ollama >> ep_s3 >> s3
        n8n_workers >> nat

        # Config, secrets, observability
        gateway >> secrets
        model_gw >> ssm
        gateway >> cw
        ollama >> cw
        cfn >> iam


if __name__ == "__main__":
    build()
    print("wrote docs/architecture/diagrams/aws-architecture.png")
