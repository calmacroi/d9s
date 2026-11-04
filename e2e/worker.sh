i=0
while true; do
  i=$((i + 1))
  echo "tick $i INFO working"
  if [ $((i % 3)) -eq 0 ]; then echo "tick $i ERROR simulated failure"; fi
  sleep 1
done
