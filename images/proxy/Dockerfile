FROM mitmproxy/mitmproxy:12.1.1

RUN pip install --no-cache-dir graphql-core==3.2.6
COPY policy.py outpost_proxy.py test_policy.py /opt/outpost/
WORKDIR /opt/outpost
ENTRYPOINT ["mitmdump", "-q", "-s", "/opt/outpost/outpost_proxy.py", \
  "--mode", "regular@8080", "--mode", "regular@8081", \
  "--set", "confdir=/data/ca", "--set", "block_global=false", \
  "--set", "connection_strategy=lazy"]
