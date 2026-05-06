FROM python:3.12-slim

# Run as non-root for safety
RUN useradd -r -u 1000 -m flowscope

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app.py ./
COPY snmp_crypto.py ./
COPY snmp_mock.py ./
COPY snmp_client.py ./
COPY web/ ./web/
COPY synth_flows.py ./

# Persistent SQLite lives here; mount a volume to keep it across restarts
RUN mkdir -p /data && chown flowscope:flowscope /data
ENV FLOWSCOPE_DB_PATH=/data/flowscope.db

USER flowscope

EXPOSE 8080/tcp
EXPOSE 2055/udp
EXPOSE 6343/udp

CMD ["python", "app.py"]
